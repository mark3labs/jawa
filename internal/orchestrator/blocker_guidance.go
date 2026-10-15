package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
)

// A build blocker parks its run (BONNIE waiting) instead of ending it. The
// operator answers with guidance through the durable workflow_interactions
// outbox, which BONNIE's Answer delivers into the same run and conversation.
// Cancelling the parked run remains the way to abandon it before Retry/Reset.

const guidanceQueuedNotice = "Guidance sent; waiting for the agent to continue."

const maxGuidanceBytes = 64000

// BlockerGuidance is the presentation of a build run waiting for guidance.
type BlockerGuidance struct {
	CardID, TaskID, Token, Reason string
	Queued                        bool
	Cancelling                    bool
}

// pendingBlocker returns the open blocker interaction of a build task, if any.
// Queued means guidance is saved but not yet delivered to the agent.
func pendingBlocker(tx *sql.Tx, task string) (BlockerGuidance, bool, error) {
	var b BlockerGuidance
	var raw, status string
	err := tx.QueryRow(`SELECT i.report_id,i.status,r.request_json FROM workflow_interactions i JOIN typed_reports r ON r.report_id=i.report_id WHERE i.task_id=? AND i.status IN ('pending','queued')`, task).Scan(&b.Token, &status, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	if err != nil {
		return b, false, err
	}
	var r reportprotocol.Request
	if err = json.Unmarshal([]byte(raw), &r); err != nil {
		return b, false, err
	}
	b.TaskID, b.Reason, b.Queued = task, r.Payload.Reason, status == "queued"
	return b, true, nil
}

// blockerNotice is the attempt error shown while a blocker is open.
func (b BlockerGuidance) notice() string {
	if b.Queued {
		return guidanceQueuedNotice
	}
	return b.Reason
}

// guidedStatus keeps a parked blocker visible while status events/snapshots
// for its still-live run arrive. A run that stops cannot receive guidance, so
// its open interaction is cancelled and the normal terminal state applies.
func guidedStatus(tx *sql.Tx, task, runState, state, reason string) (string, string, error) {
	b, open, err := pendingBlocker(tx, task)
	if err != nil || !open {
		return state, reason, err
	}
	switch runState {
	case "running", "waiting", "pending":
		return "blocked", b.notice(), nil
	case "completed":
		// An agent that cannot park (older host) ends with its blocker. Its
		// natural outcome closes the interaction and keeps the reason.
		return "blocked", b.Reason, nil
	case "failed", "cancelled", "retired":
		_, err = tx.Exec(`UPDATE workflow_interactions SET status='cancelled' WHERE task_id=? AND status IN ('pending','queued')`, task)
	}
	return state, reason, err
}

// blockerOutcome applies a natural outcome for a build task with blocker
// interactions. It reports whether the outcome was fully handled.
func blockerOutcome(tx *sql.Tx, a Attempt, out client.Outcome, data string) (bool, error) {
	if out.State == runtime.RunWaiting && out.Suspend != nil {
		var reportID, status, raw string
		err := tx.QueryRow(`SELECT i.report_id,i.status,r.request_json FROM workflow_interactions i JOIN typed_reports r ON r.report_id=i.report_id WHERE i.task_id=? AND r.tool_call_id=?`, a.TaskID, out.Suspend.ToolCallID).Scan(&reportID, &status, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return true, err
		}
		// Answered/cancelled suspensions are stale replays of an earlier turn.
		if status != "pending" && status != "queued" {
			return true, nil
		}
		var r reportprotocol.Request
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			return true, err
		}
		if out.Version != 1 || out.Error != "" || out.AnswerSubject != "bonnie.answers."+out.AgentID || r.AgentID != out.AgentID || r.RunID != out.RunID || r.AttemptID != out.AttemptID || r.TurnID != out.Suspend.TurnID {
			return true, nil
		}
		if _, err = tx.Exec(`UPDATE workflow_interactions SET waiting_json=? WHERE report_id=? AND status IN ('pending','queued')`, data, reportID); err != nil {
			return true, err
		}
		notice := r.Payload.Reason
		if status == "queued" {
			notice = guidanceQueuedNotice
		}
		_, err = tx.Exec(`UPDATE workflow_attempts SET agent_id=?,run_id=?,remote_attempt_id=?,state='blocked',run_state='waiting',error=?,updated_at=? WHERE id=?`, out.AgentID, out.RunID, out.AttemptID, notice, time.Now().UnixMilli(), a.ID)
		return true, err
	}
	b, open, err := pendingBlocker(tx, a.TaskID)
	if err != nil || !open {
		return false, err
	}
	switch out.State {
	case runtime.RunCompleted, runtime.RunFailed, runtime.RunCancelled:
	default:
		return false, nil
	}
	if _, err = tx.Exec(`UPDATE workflow_interactions SET status='cancelled' WHERE task_id=? AND status IN ('pending','queued')`, a.TaskID); err != nil {
		return true, err
	}
	if out.State != runtime.RunCompleted || out.Error != "" {
		return false, nil
	}
	// The run ended instead of parking (an older agent): keep the blocker as the
	// terminal reason so fresh Retry and Reset remain available.
	_, err = tx.Exec(`UPDATE workflow_attempts SET agent_id=?,run_id=?,remote_attempt_id=?,state='blocked',run_state=?,result=?,outcome_json=?,error=?,updated_at=? WHERE id=?`, out.AgentID, out.RunID, out.AttemptID, string(out.State), out.Response, data, b.Reason, time.Now().UnixMilli(), a.ID)
	return true, err
}

// GuideBlocked queues operator guidance for the latest build run parked on a
// blocker. expected binds the form to the blocker the operator actually read.
func (w *Workflow) GuideBlocked(cardID, text, expected string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("guidance is required")
	}
	if len(text) > maxGuidanceBytes {
		return errors.New("guidance is too long")
	}
	if expected == "" {
		return errors.New("refresh before replying to the blocker")
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var lane string
	if err = tx.QueryRow(`SELECT status FROM cards WHERE id=?`, cardID).Scan(&lane); err != nil {
		return workflowCardError(err)
	}
	if lane != "Building" && lane != "Fix" {
		return errors.New("guidance requires a Building or Fix card")
	}
	a, err := scanAttempt(tx.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE card_id=? ORDER BY number DESC LIMIT 1`, cardID))
	if err != nil {
		return err
	}
	var cancelling bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workflow_cancel_requests WHERE task_id=? AND status IN ('pending','requested'))`, a.TaskID).Scan(&cancelling); err != nil {
		return err
	}
	if cancelling {
		return errors.New("cancellation requested; guidance cannot be delivered")
	}
	if err = queueBlockedReply(tx, a, expected, text); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	w.logAttempt(log.InfoLevel, "Blocker guidance queued", a)
	return nil
}

func queueBlockedReply(tx *sql.Tx, a Attempt, expected, text string) error {
	res, err := tx.Exec(`UPDATE workflow_interactions SET status='queued',reply=? WHERE report_id=? AND task_id=? AND status='pending'`, text, expected, a.TaskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("blocker changed or was already answered; refresh before replying")
	}
	if _, err = tx.Exec(`UPDATE workflow_attempts SET error=?,updated_at=? WHERE id=? AND state='blocked'`, guidanceQueuedNotice, time.Now().UnixMilli(), a.ID); err != nil {
		return err
	}
	return nil
}

// dispatchBlockerGuidance delivers queued guidance through BONNIE Answer once
// the trusted waiting outcome (route and suspension turn) is durable. Failures
// stay queued across restart; a missing outcome is never guessed.
func (w *Workflow) dispatchBlockerGuidance() {
	if w.client == nil {
		return
	}
	rows, err := w.s.db.Query(`SELECT report_id FROM workflow_interactions WHERE status='queued' AND waiting_json<>'' ORDER BY created_at`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	// Deliver what was read; a partial list is retried on the next pass.
	if err = rows.Err(); err != nil {
		w.log().Warn("Failed to read queued blocker guidance; will retry")
	}
	_ = rows.Close()
	for _, id := range ids {
		w.deliverBlockerGuidance(id)
	}
}

func (w *Workflow) deliverBlockerGuidance(id string) {
	tx, err := w.s.writeTx()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	var raw, text, task string
	if err = tx.QueryRow(`SELECT waiting_json,reply,task_id FROM workflow_interactions WHERE report_id=? AND status='queued'`, id).Scan(&raw, &text, &task); err != nil {
		return
	}
	var out client.Outcome
	if err = json.Unmarshal([]byte(raw), &out); err != nil || out.Suspend == nil {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 2*time.Second)
	_, err = w.client.Answer(ctx, out, []runtime.InputResponse{{TurnID: out.Suspend.TurnID, Text: text}})
	cancel()
	if err != nil {
		if w.ctx.Err() == nil {
			w.log().Warn("Blocker guidance delivery failed; will retry", "task_id", task)
		}
		return
	}
	now := time.Now().UnixMilli()
	if _, err = tx.Exec(`UPDATE workflow_interactions SET status='answered',answered_at=? WHERE report_id=? AND status='queued'`, now, id); err != nil {
		return
	}
	if _, err = tx.Exec(`UPDATE workflow_attempts SET state='running',run_state='running',error='',updated_at=? WHERE task_id=? AND state='blocked' AND ready=0`, now, task); err != nil {
		return
	}
	if tx.Commit() == nil {
		w.log().Info("Blocker guidance delivered", "task_id", task)
	}
}

// blockerGuidance lists open build blockers by card for presentation.
func (s *Store) blockerGuidance() (map[string]BlockerGuidance, error) {
	rows, err := s.db.Query(`SELECT a.card_id,i.task_id,i.report_id,i.status,r.request_json,EXISTS(SELECT 1 FROM workflow_cancel_requests cr WHERE cr.task_id=i.task_id AND cr.status IN ('pending','requested')) FROM workflow_interactions i JOIN typed_reports r ON r.report_id=i.report_id JOIN workflow_attempts a ON a.task_id=i.task_id WHERE i.status IN ('pending','queued') AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=a.card_id)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[string]BlockerGuidance{}
	for rows.Next() {
		var b BlockerGuidance
		var status, raw string
		if err = rows.Scan(&b.CardID, &b.TaskID, &b.Token, &status, &raw, &b.Cancelling); err != nil {
			return nil, err
		}
		var r reportprotocol.Request
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, err
		}
		b.Reason, b.Queued = r.Payload.Reason, status == "queued"
		result[b.CardID] = b
	}
	return result, rows.Err()
}
