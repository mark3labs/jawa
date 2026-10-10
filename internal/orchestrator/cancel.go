package orchestrator

import (
	"context"
	"errors"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
)

const cancelErrorText = "Cancellation could not be confirmed; check agent status before retrying."

func migrateCancellation(s *Store) error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS workflow_cancel_requests (
 id INTEGER PRIMARY KEY, task_id TEXT, agent_id TEXT, run_id TEXT, remote_attempt_id TEXT, turn_id TEXT,
 status TEXT, error TEXT, created_at INTEGER)`)
	return err
}

// CancelCard requests cancellation only for the latest attempt after observing its exact active turn.
func (w *Workflow) CancelCard(ctx context.Context, cardID string) error {
	if ctx == nil {
		ctx = w.ctx
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.client == nil {
		return errors.New("agent control unavailable")
	}
	if err := migrateCancellation(w.s); err != nil {
		return err
	}
	a, err := w.latestCancelAttempt(cardID)
	if err != nil {
		return err
	}
	if err := w.checkCancelLatest(a); err != nil {
		return err
	}
	if a.AgentID == "" || a.RunID == "" || a.RemoteAttemptID == "" {
		return errors.New("cancellation target unavailable")
	}
	target := client.Target{TaskID: a.TaskID, AgentID: a.AgentID, RunID: a.RunID, AttemptID: a.RemoteAttemptID}
	queryCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	observed, err := w.client.Status(queryCtx, target)
	stop()
	if err != nil || observed.Version != 1 || observed.Target != target || observed.TurnID == "" || observed.Error != "" {
		return w.cancelRecordFailure(a, "uncertain", cancelErrorText)
	}
	if observed.State == "cancelled" || observed.State == "failed" || observed.State == "completed" {
		return w.applyStatus(a, observed)
	}
	// Waiting/interrupted turns can be durably cancelled even when no local
	// model call is active. The observed TurnID still scopes the command.
	if !observed.Active && observed.State != "waiting" && observed.State != "interrupted" && observed.State != "running" {
		return w.cancelRecordFailure(a, "uncertain", cancelErrorText)
	}
	if err = w.checkCancelLatest(a); err != nil {
		return err
	}
	if err = w.cancelAudit(a, observed.TurnID, "pending", ""); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	reply, callErr := w.client.CancelTurn(callCtx, target, observed.TurnID)
	cancel()
	if callErr != nil || reply.Version != 1 || reply.Target != target || reply.TurnID != observed.TurnID || reply.Error != "" {
		return w.cancelRecordFailure(a, "uncertain", cancelErrorText)
	}
	if reply.State == "cancelled" {
		if err = w.applyStatus(a, reply); err != nil {
			return err
		}
		return w.cancelAudit(a, observed.TurnID, "cancelled", "")
	}
	if reply.CancelStatus != "requested" {
		return w.cancelRecordFailure(a, "uncertain", cancelErrorText)
	}
	return w.recordCancelNotice(a, observed.TurnID, "requested", "Cancellation requested; waiting for the agent to confirm it stopped.")
}

func (w *Workflow) latestCancelAttempt(cardID string) (Attempt, error) {
	return scanAttempt(w.s.db.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE card_id=? ORDER BY number DESC LIMIT 1`, cardID))
}
func (w *Workflow) checkCancelLatest(a Attempt) error {
	var building bool
	if err := w.s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM cards WHERE id=? AND status='Building')`, a.CardID).Scan(&building); err != nil {
		return err
	}
	current, err := w.latestCancelAttempt(a.CardID)
	if !building || err != nil || current.Ready || !attemptActive(current) || current.ID != a.ID || current.TaskID != a.TaskID || current.AgentID != a.AgentID || current.RunID != a.RunID || current.RemoteAttemptID != a.RemoteAttemptID {
		return errors.New("cancellation target changed; no request sent")
	}
	return nil
}
func (w *Workflow) cancelAudit(a Attempt, turnID, status, msg string) error {
	_, err := w.s.db.Exec(`INSERT INTO workflow_cancel_requests(task_id,agent_id,run_id,remote_attempt_id,turn_id,status,error,created_at) VALUES(?,?,?,?,?,?,?,?)`, a.TaskID, a.AgentID, a.RunID, a.RemoteAttemptID, turnID, status, msg, time.Now().UnixMilli())
	return err
}
func (w *Workflow) cancelRecordFailure(a Attempt, status, msg string) error {
	if err := w.recordCancelNotice(a, "", status, msg); err != nil {
		return err
	}
	return errors.New(msg)
}

func (w *Workflow) recordCancelNotice(a Attempt, turnID, status, msg string) error {
	if err := w.cancelAudit(a, turnID, status, msg); err != nil {
		return err
	}
	_, err := w.s.db.Exec(`UPDATE workflow_attempts SET error=?,updated_at=? WHERE id=? AND ready=0 AND run_state IN ('pending','running','waiting','interrupted') AND outcome_json=? AND error<>? AND number=(SELECT max(number) FROM workflow_attempts WHERE card_id=?)`, msg, time.Now().UnixMilli(), a.ID, a.OutcomeJSON, executionConflictNotice, a.CardID)
	return err
}
