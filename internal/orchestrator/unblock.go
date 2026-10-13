package orchestrator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
)

const unblockPrompt = "Resume this task in the existing run and branch. Finish the requested work within the approved scope, create or attach its real pull request, and call completeTask with the positive PR number. Do not merge or claim completion without a PR. Preserve the existing authorization and bounded-work instructions. If genuinely unable to finish, reportBlocker with the remaining reason."

func attachedPR(a Attempt) string {
	if a.PRURL != "" {
		return a.PRURL
	}
	return a.RepairPRURL
}
func unblockToken(a Attempt, lane string) string {
	return fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "%s\n%d\n%s\n%s\n%s\n%d\n%s\n%s", a.ID, a.ReportGeneration, lane, a.State, a.OutcomeJSON, a.EventSeq, attachedPR(a), a.Error)))
}

// Validate stored repository assignment rather than accepting a browser URL.
func validateAttachedPR(a Attempt, provider, repo string) (string, error) {
	pr := attachedPR(a)
	if pr == "" || (a.RepairPRURL != "" && a.PRURL != "" && a.RepairPRURL != a.PRURL) {
		return "", errors.New("missing or conflicting assigned pull request")
	}
	n, err := strconv.Atoi(pr[strings.LastIndex(pr, "/")+1:])
	if err != nil {
		return "", errors.New("invalid assigned pull request")
	}
	expected, err := projectPRURL(provider, repo, n)
	if err != nil || expected != pr {
		return "", errors.New("assigned pull request does not match the project")
	}
	return pr, nil
}

// Both explicit actions keep ownership safety even when bypassing PR verification.
func (w *Workflow) unblockSnapshot(tx *sql.Tx, id, expected string) (Attempt, string, string, string, error) {
	var lane, project, provider, repo string
	a, err := scanAttempt(tx.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE card_id=? ORDER BY number DESC LIMIT 1`, id))
	if err != nil {
		return a, "", "", "", err
	}
	if err = tx.QueryRow(`SELECT c.status,c.project_id,p.provider,p.repo FROM cards c JOIN projects p ON p.id=c.project_id WHERE c.id=?`, id).Scan(&lane, &project, &provider, &repo); err != nil {
		return a, "", "", "", err
	}
	if expected == "" || expected != unblockToken(a, lane) {
		return a, "", "", "", errors.New("blocked task changed; refresh before unblocking")
	}
	if (lane != "Building" && lane != "Fix") || a.State != "blocked" || a.Ready || a.AgentID == "" || a.RunID == "" || a.RemoteAttemptID == "" || (a.RunState != "completed" && a.RunState != "failed") {
		return a, "", "", "", errors.New("unblock requires an owned terminal blocked Building or Fix task")
	}
	if err = noActiveAttempts(tx, id); err != nil {
		return a, "", "", "", err
	}
	var unsafe bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workflow_conflicts WHERE task_id IN (SELECT task_id FROM workflow_attempts WHERE card_id=?)) OR EXISTS(SELECT 1 FROM workflow_cancel_requests WHERE task_id IN (SELECT task_id FROM workflow_attempts WHERE card_id=?) AND status IN ('pending','requested','uncertain'))`, id, id).Scan(&unsafe); err != nil {
		return a, "", "", "", err
	}
	if unsafe {
		return a, "", "", "", errors.New("execution conflict or unresolved cancellation prevents unblock")
	}
	return a, lane, project, provider + "\n" + repo, nil
}
func (w *Workflow) confirmUnblockOwner(a Attempt) (client.Status, error) {
	var status client.Status
	if w.client == nil {
		return status, errors.New("owning agent unavailable")
	}
	target := client.Target{TaskID: a.TaskID, AgentID: a.AgentID, RunID: a.RunID, AttemptID: a.RemoteAttemptID}
	ctx, cancel := context.WithTimeout(w.ctx, time.Second)
	defer cancel()
	status, err := w.client.Status(ctx, target)
	if err != nil || status.Version != 1 || status.Error != "" || status.Target != target || status.Active || status.CancelRequested || status.TurnID == "" || status.Seq < a.EventSeq || (status.State != runtime.RunCompleted && status.State != runtime.RunFailed) {
		return status, errors.New("owning agent cannot confirm an idle completed or failed run; unblock refused")
	}
	return status, nil
}

func (w *Workflow) Unblock(id, expected, prompt string) error {
	prompt = strings.TrimSpace(prompt)
	if !utf8.ValidString(prompt) || len(prompt) < 1 || len(prompt) > 2048 {
		return errors.New("unblock prompt must contain 1–2048 UTF-8 bytes")
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, _, _, _, err := w.unblockSnapshot(tx, id, expected)
	if err != nil {
		return err
	}
	if attachedPR(a) != "" {
		return errors.New("task has an assigned PR; use Force ready")
	}
	var blocker string
	err = tx.QueryRow(`SELECT report_id FROM typed_reports WHERE task_id=? AND generation=? AND kind='report_blocker'`, a.TaskID, a.ReportGeneration).Scan(&blocker)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	reported, err := acceptedReport(tx, a.TaskID)
	if err != nil {
		return err
	}
	if reported && blocker == "" {
		return errors.New("accepted completion cannot be reopened")
	}
	status, err := w.confirmUnblockOwner(a)
	if err != nil {
		return err
	}
	req := reportprotocol.FailedResume{Version: 1, ID: uuid.NewString(), Target: status.Target, Seq: status.Seq, TurnID: status.TurnID, Mode: "unblock", Generation: a.ReportGeneration + 1, Prompt: prompt, BlockerReportID: blocker}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(raw) > 4096 {
		return errors.New("unblock authorization exceeds host payload limit")
	}
	snapshot, err := json.Marshal(a)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err = tx.Exec(`INSERT INTO failed_resumes(id,task_id,card_id,planning,request_json,previous_outcome,created_at,mode,generation,previous_snapshot,blocker_report_id) VALUES(?,?,?,0,?,?,?,'unblock',?,?,?)`, req.ID, a.TaskID, id, string(raw), a.OutcomeJSON, now, req.Generation, string(snapshot), blocker); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE typed_reports SET superseded_by=? WHERE report_id=?`, req.ID, blocker); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM auto_resumes WHERE task_id=?`, a.TaskID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE workflow_attempts SET report_generation=?,report_turn='',state='running',run_state='running',outcome_json='',result='',error=?,event_seq=?,updated_at=? WHERE id=?`, req.Generation, resumePendingNotice, status.Seq, now, a.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (w *Workflow) ForceReady(id, expected, operator string, acknowledged bool) error {
	if !acknowledged {
		return errors.New("confirm that CI, review and readiness verification are bypassed")
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var duplicate bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workflow_ready_overrides o JOIN workflow_attempts a ON a.id=o.attempt_id JOIN cards c ON c.id=a.card_id WHERE c.id=? AND o.action_token=? AND a.ready=1 AND a.state='ready' AND c.status='Done' AND a.report_generation=o.generation AND a.pr_url=o.pr_url AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id))`, id, expected).Scan(&duplicate); err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	a, lane, project, config, err := w.unblockSnapshot(tx, id, expected)
	if err != nil {
		return err
	}
	parts := strings.SplitN(config, "\n", 2)
	pr, err := validateAttachedPR(a, parts[0], parts[1])
	if err != nil {
		return err
	}
	if _, err = w.confirmUnblockOwner(a); err != nil {
		return err
	}
	snapshot, err := json.Marshal(a)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err = tx.Exec(`INSERT INTO workflow_ready_overrides(attempt_id,pr_url,generation,action_token,prior_snapshot,operator,marked_at) VALUES(?,?,?,?,?,?,?)`, a.ID, pr, a.ReportGeneration, expected, string(snapshot), operator, now); err != nil {
		return err
	}
	if err = moveWorkflowCard(tx, id, project, lane, "Done", int(^uint(0)>>1)); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE workflow_attempts SET ready=1,state='ready',pr_url=?,error='',updated_at=? WHERE id=?`, pr, now, a.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM auto_resumes WHERE card_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
