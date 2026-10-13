package orchestrator

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/nats-io/nats.go"
	"jawa/internal/orchestrator/db/sqlc"
	"jawa/internal/reportprotocol"
)

func acceptedReport(tx *sql.Tx, task string) (bool, error) {
	var found bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM typed_reports WHERE task_id=? AND generation=COALESCE((SELECT report_generation FROM workflow_attempts WHERE task_id=?),0) AND kind IN ('complete_task','report_blocker'))`, task, task).Scan(&found)
	return found, err
}

func (w *Workflow) subscribeReports(conn *nats.Conn) error {
	sub, err := conn.QueueSubscribeSync(reportprotocol.Subject("bonnie"), "jawa-typed-reports")
	if err != nil {
		return err
	}
	if err = conn.Flush(); err != nil {
		_ = sub.Unsubscribe()
		return err
	}
	w.reports = sub
	// Join report handling before closing the store, including an in-flight commit.
	w.wg.Go(func() {
		for w.ctx.Err() == nil {
			msg, err := sub.NextMsgWithContext(w.ctx)
			if err != nil {
				return
			}
			w.submitReportMessage(msg)
		}
	})
	return nil
}

func (w *Workflow) submitReportMessage(msg *nats.Msg) {
	var r reportprotocol.Request
	ack := reportprotocol.Ack{Version: reportprotocol.Version}
	var err error
	if len(msg.Data) > 512*1024 {
		err = errors.New("typed report exceeds limit")
	} else {
		dec := json.NewDecoder(bytes.NewReader(msg.Data))
		dec.DisallowUnknownFields()
		err = dec.Decode(&r)
		if err == nil {
			var extra any
			if dec.Decode(&extra) != io.EOF {
				err = errors.New("multiple report values")
			}
		}
	}
	ack.ReportID = r.ReportID
	if err == nil {
		ack, err = w.acceptReport(w.ctx, r)
	}
	if err != nil {
		ack.Error = err.Error()
	}
	data, _ := json.Marshal(ack)
	_ = msg.Respond(data)
}

// acceptReport commits the inbox and transition together. Duplicate replay uses
// the original complete identity (including turn) before live runtime checks.
func (w *Workflow) acceptReport(ctx context.Context, r reportprotocol.Request) (reportprotocol.Ack, error) {
	ack := reportprotocol.Ack{Version: reportprotocol.Version, ReportID: r.ReportID}
	if err := r.Validate(); err != nil {
		return ack, err
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return ack, err
	}
	defer func() { _ = tx.Rollback() }()
	var stored string
	err = tx.QueryRow(`SELECT request_json FROM typed_reports WHERE report_id=? OR (task_id=? AND tool_call_id=?)`, r.ReportID, r.TaskID, r.ToolCallID).Scan(&stored)
	if err == nil {
		var previous reportprotocol.Request
		if json.Unmarshal([]byte(stored), &previous) != nil {
			return ack, errors.New("invalid stored report receipt")
		}
		if previous.ReportID != r.ReportID || previous.Identity != r.Identity {
			return ack, errors.New("typed report conflicts with accepted report or owner")
		}
		ack.Accepted, ack.Duplicate = true, true
		return ack, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ack, err
	}

	t, planningErr := sqlc.New(tx).GetPlanningTurn(ctx, r.TaskID)
	if planningErr != nil && !errors.Is(planningErr, sql.ErrNoRows) {
		return ack, planningErr
	}
	planning := planningErr == nil
	var a Attempt
	if planning {
		var sessionState string
		if err = tx.QueryRow(`SELECT state FROM planning_sessions WHERE id=?`, t.SessionID).Scan(&sessionState); err != nil {
			return ack, err
		}
		if r.Kind == reportprotocol.CompleteTask && sessionState != "approved" {
			return ack, errors.New("completeTask is not allowed before planning approval")
		}
		current, err := planningTurnCurrent(tx, t)
		if err != nil {
			return ack, err
		}
		if sessionState == "approved" && (r.Kind == reportprotocol.CompleteTask || r.Kind == reportprotocol.ReportBlocker) {
			current = t.State != "cancelled" && t.OutcomeJson == ""
		}
		var pending bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM planning_interactions WHERE task_id=? AND status IN ('pending','queued'))`, t.ID).Scan(&pending); err != nil {
			return ack, err
		}
		if pending || !current || t.OutcomeJson != "" || !planningIdentityMatches(t, r.AgentID, r.RunID, r.AttemptID) {
			return ack, errors.New("planning task is stale, terminal, cancelled or owned by another execution")
		}
	} else {
		a, err = scanAttempt(tx.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE task_id=?`, r.TaskID))
		if err != nil {
			return ack, fmt.Errorf("unknown build task: %w", err)
		}
		if r.Kind == reportprotocol.ProposeSpec || r.Kind == reportprotocol.Clarify {
			return ack, errors.New("proposeSpec is not allowed during build")
		}
		var current bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM cards WHERE id=? AND status IN ('Building','Fix') AND ?=(SELECT max(number) FROM workflow_attempts WHERE card_id=?))`, a.CardID, a.Number, a.CardID).Scan(&current); err != nil {
			return ack, err
		}
		if !current || a.Ready || (a.OutcomeJSON != "" && !resumableOutcome(a)) || a.RunState == "failed" || a.RunState == "cancelled" || !matches(a, r.AgentID, r.RunID, r.AttemptID) {
			return ack, errors.New("build task is stale, terminal, cancelled or owned by another execution")
		}
		var cancelling bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workflow_cancel_requests WHERE task_id=? AND status IN ('pending','requested','uncertain'))`, a.TaskID).Scan(&cancelling); err != nil {
			return ack, err
		}
		if cancelling {
			return ack, errors.New("build task cancellation requested")
		}
	}
	// BONNIE's NATS Status queries its runtime Observe snapshot, not model text.
	// It also resolves admission when the durable accepted event has not arrived yet.
	if w.observeReport == nil {
		return ack, errors.New("trusted runtime observation unavailable")
	}
	target := client.Target{TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID}
	observeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	status, err := w.observeReport(observeCtx, target)
	cancel()
	if err != nil {
		return ack, fmt.Errorf("observe report owner: %w", err)
	}
	if status.Version != 1 || status.Error != "" || status.Target != target || status.TurnID != r.TurnID || !status.Active || string(status.State) != "running" {
		return ack, errors.New("typed report does not match active BONNIE task/run/attempt/turn")
	}

	generation := 0
	if !planning {
		generation = a.ReportGeneration
		if generation > 0 && a.ReportTurn == "" {
			var raw string
			if err = tx.QueryRow(`SELECT request_json FROM failed_resumes WHERE task_id=? ORDER BY created_at DESC LIMIT 1`, a.TaskID).Scan(&raw); err != nil {
				return ack, err
			}
			var authorization reportprotocol.FailedResume
			if json.Unmarshal([]byte(raw), &authorization) != nil || authorization.Target != target || status.Seq <= authorization.Seq || r.TurnID == authorization.TurnID {
				return ack, errors.New("report does not belong to authorized unblock turn")
			}
			if _, err = tx.Exec(`UPDATE workflow_attempts SET report_turn=? WHERE id=? AND report_generation=?`, r.TurnID, a.ID, generation); err != nil {
				return ack, err
			}
		} else if generation > 0 && a.ReportTurn != r.TurnID {
			return ack, errors.New("report belongs to a previous unblock turn")
		}
	}
	now := time.Now().UnixMilli()
	raw, err := json.Marshal(r)
	if err != nil {
		return ack, err
	}
	if _, err = tx.Exec(`INSERT INTO typed_reports(task_id,report_id,tool_call_id,agent_id,run_id,attempt_id,turn_id,kind,request_json,accepted_at,generation) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, r.TaskID, r.ReportID, r.ToolCallID, r.AgentID, r.RunID, r.AttemptID, r.TurnID, r.Kind, string(raw), now, generation); err != nil {
		return ack, err
	}
	if planning {
		err = persistPlanningReport(tx, ctx, t, r, string(raw), now)
	} else {
		pr, reason := "", r.Payload.Reason
		if r.Kind == reportprotocol.CompleteTask {
			var provider, repo string
			err = tx.QueryRow(`SELECT p.provider,p.repo FROM cards c JOIN projects p ON p.id=c.project_id WHERE c.id=?`, a.CardID).Scan(&provider, &repo)
			if err == nil {
				pr, err = projectPRURL(provider, repo, r.Payload.PRNumber)
			}
			if err == nil && a.RepairPRURL != "" && pr != a.RepairPRURL {
				err = errors.New("repair must complete the existing pull request")
			}
			reason = "awaiting verification"
		}
		if err == nil {
			_, err = tx.Exec(`UPDATE workflow_attempts SET agent_id=?,run_id=?,remote_attempt_id=?,state='blocked',run_state='completed',result=?,outcome_json=?,error=?,pr_url=?,updated_at=? WHERE id=?`, r.AgentID, r.RunID, r.AttemptID, string(raw), string(raw), reason, pr, now, a.ID)
		}
	}
	if err != nil {
		return ack, err
	}
	if err = tx.Commit(); err != nil {
		return ack, err
	}
	ack.Accepted = true
	return ack, nil
}

func persistPlanningReport(tx *sql.Tx, ctx context.Context, t sqlc.PlanningTurn, r reportprotocol.Request, raw string, now int64) error {
	p, err := sqlc.New(tx).GetPlanningSessionByID(ctx, t.SessionID)
	if err != nil {
		return err
	}
	if r.Kind == reportprotocol.CompleteTask {
		_, err = tx.Exec(`UPDATE planning_turns SET state='completed',result=?,outcome_json=?,error='',updated_at=? WHERE id=?`, raw, raw, now, t.ID)
		return err // Approved snapshot and the distinct build are immutable here.
	}
	state, session, reason := "waiting", "ready", ""
	message := r.Payload.Spec
	terminal := ""
	switch r.Kind {
	case reportprotocol.ReportBlocker:
		state, session, reason = "failed", "failed", r.Payload.Reason
		message, terminal = reason, raw
	case reportprotocol.Clarify:
		session = "questions"
		for _, question := range r.Payload.Questions {
			message += question + "\n"
		}
	default:
		if p.RepoCommit != "" && p.RepoCommit != r.Payload.RepoCommit {
			return errors.New("planner inspected a different commit than the pinned session")
		}
		var revision int64
		if err = tx.QueryRow(`SELECT coalesce(max(r.number),0)+1 FROM planning_revisions r JOIN planning_sessions s ON s.id=r.session_id WHERE s.card_id=?`, p.CardID).Scan(&revision); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO planning_revisions(session_id,number,spec,questions_json,repo_commit,created_at) VALUES(?,?,?,'[]',?,?)`, p.ID, revision, r.Payload.Spec, r.Payload.RepoCommit, now); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE planning_sessions SET spec=?,repo_commit=?,revision=? WHERE id=?`, r.Payload.Spec, r.Payload.RepoCommit, revision, p.ID); err != nil {
			return err
		}
	}
	if terminal == "" {
		if _, err = tx.Exec(`INSERT INTO planning_interactions(report_id,task_id,created_at) VALUES(?,?,?)`, r.ReportID, t.ID, now); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE planning_turns SET state=?,agent_id=?,run_id=?,remote_attempt_id=?,result=?,outcome_json=?,error=?,updated_at=? WHERE id=?`, state, r.AgentID, r.RunID, r.AttemptID, message, terminal, reason, now, t.ID); err != nil {
		return err
	}
	if err = planningMessage(tx, p.ID, "assistant", message); err != nil {
		return err
	}
	if p.State == "approved" {
		return nil
	}
	_, err = tx.Exec(`UPDATE planning_sessions SET state=?,error=?,updated_at=? WHERE id=?`, session, reason, now, p.ID)
	return err
}
