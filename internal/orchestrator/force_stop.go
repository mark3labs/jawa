package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/charmbracelet/log"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
)

// Force stop is the operator's escape hatch. Every other build action waits
// for evidence from the owning agent (a confirmed stop, a terminal report, a
// resume receipt). When that agent is gone, restarted under a new identity, or
// wedged, the evidence never arrives and the card would otherwise be stuck.
// Force stop revokes local authority without that evidence: it fences the
// attempt so later events, results, typed reports and resume receipts are
// ignored, then asks the agent to stop on a best-effort basis. It never claims
// the remote run stopped and cannot undo commits or pull requests.

const (
	abandonedRunState = "abandoned"
	forceStopNotice   = "Force-stopped by operator. The agent was not confirmed stopped; later reports from this run are ignored."
)

// forceStoppable reports whether an attempt still holds execution authority
// that no other action can release: active or resuming work, or a latest
// attempt that is neither ready nor a terminal failure (Retry/Reset need one).
func forceStoppable(a Attempt, latest bool) bool {
	if a.Ready || a.State == "ready" || a.RunState == abandonedRunState {
		return false
	}
	return attemptActive(a) || a.Error == resumePendingNotice || (latest && !terminalFailure(a))
}

// ForceStop fences all attempts of a card that still hold authority, abandons
// pending resumes and guidance, and leaves the card in its lane so Retry,
// Reset or Delete become available. Planning uses CancelPlanning instead.
func (w *Workflow) ForceStop(cardID string) (retErr error) {
	w.log().Info("workflow force stop requested", "card_id", cardID)
	defer func() {
		if retErr != nil {
			w.log().Warn("workflow force stop failed", "card_id", cardID, "error", retErr)
		}
	}()
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if err := migrateCancellation(w.s); err != nil {
		return err
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	if err = tx.QueryRow(`SELECT status FROM cards WHERE id=?`, cardID).Scan(&status); err != nil {
		return workflowCardError(err)
	}
	if status == "Planning" {
		return errors.New("use Cancel planning to stop a planning run")
	}
	rows, err := tx.Query(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE card_id=? ORDER BY number`, cardID)
	if err != nil {
		return err
	}
	var all []Attempt
	for rows.Next() {
		a, e := scanAttempt(rows)
		if e != nil {
			_ = rows.Close()
			return e
		}
		all = append(all, a)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	var targets []Attempt
	for i, a := range all {
		if forceStoppable(a, i == len(all)-1) {
			targets = append(targets, a)
		}
	}
	res, err := tx.Exec(`UPDATE failed_resumes SET status='abandoned' WHERE card_id=? AND status='pending'`, cardID)
	if err != nil {
		return err
	}
	resumes, _ := res.RowsAffected()
	if len(targets) == 0 && resumes == 0 {
		return errors.New("nothing to force stop; use Retry, Reset or Delete")
	}
	if _, err = tx.Exec(`DELETE FROM auto_resumes WHERE card_id=?`, cardID); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, a := range targets {
		// A final, non-resumable outcome is the fence every delivery path checks.
		// run_state 'abandoned' keeps it from being offered as a resumable failure.
		raw, e := json.Marshal(client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: a.AgentID, RunID: a.RunID, AttemptID: a.RemoteAttemptID, State: runtime.RunFailed, Error: forceStopNotice})
		if e != nil {
			return e
		}
		if _, err = tx.Exec(`UPDATE workflow_attempts SET state='failed',run_state=?,outcome_json=?,error=?,updated_at=? WHERE id=?`, abandonedRunState, string(raw), forceStopNotice, now, a.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE workflow_interactions SET status='cancelled' WHERE task_id=? AND status IN ('pending','queued')`, a.TaskID); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO workflow_cancel_requests(task_id,agent_id,run_id,remote_attempt_id,turn_id,status,error,created_at) VALUES(?,?,?,?,'','abandoned',?,?)`, a.TaskID, a.AgentID, a.RunID, a.RemoteAttemptID, forceStopNotice, now); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, a := range targets {
		w.logAttempt(log.WarnLevel, "Attempt force-stopped by operator; remote stop not confirmed", a)
		if a.AgentID != "" && a.RunID != "" && a.RemoteAttemptID != "" && w.client != nil {
			target := client.Target{TaskID: a.TaskID, AgentID: a.AgentID, RunID: a.RunID, AttemptID: a.RemoteAttemptID}
			w.wg.Go(func() { w.requestRemoteStop(target) })
		}
	}
	return nil
}

// requestRemoteStop asks a still-reachable agent to cancel its current turn.
// Failure is expected when the agent is gone and changes nothing locally.
func (w *Workflow) requestRemoteStop(target client.Target) {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	observed, err := w.client.Status(ctx, target)
	if err != nil || observed.Error != "" || observed.Target != target || observed.TurnID == "" {
		w.log().Debug("Force stop: agent unreachable; remote run not cancelled", "task_id", target.TaskID, "agent_id", target.AgentID)
		return
	}
	switch observed.State {
	case runtime.RunCancelled, runtime.RunFailed, runtime.RunCompleted:
		return
	}
	reply, err := w.client.CancelTurn(ctx, target, observed.TurnID)
	if err != nil || reply.Error != "" {
		w.log().Debug("Force stop: remote cancellation not accepted", "task_id", target.TaskID, "agent_id", target.AgentID)
		return
	}
	w.log().Info("Force stop: remote cancellation requested", "task_id", target.TaskID, "agent_id", target.AgentID, "state", string(reply.State))
}
