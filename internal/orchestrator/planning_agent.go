package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/orchestrator/db/sqlc"
	"jawa/internal/reportprotocol"
)

func planningIdentityMatches(t sqlc.PlanningTurn, agent, run, attempt string) bool {
	return (t.TargetAgentID == "" || t.TargetAgentID == agent) && ((t.AgentID == "" && t.RunID == "" && t.RemoteAttemptID == "") || (t.AgentID == agent && t.RunID == run && t.RemoteAttemptID == attempt))
}
func planningTurnCurrent(tx *sql.Tx, t sqlc.PlanningTurn) (bool, error) {
	var current bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM planning_sessions s JOIN cards c ON c.id=s.card_id WHERE s.id=? AND c.status='Planning' AND s.state IN ('queued','running','failed','questions','ready') AND ?=(SELECT max(number) FROM planning_turns WHERE session_id=s.id))`, t.SessionID, t.Number).Scan(&current)
	return current && (planningTurnActive(t.State) || t.State == "waiting" || (t.State == "failed" && t.OutcomeJson == "")), err
}

// Share the main BONNIE consumers, classifying task IDs while holding their transaction.
func (w *Workflow) planningEvent(tx *sql.Tx, ctx context.Context, ev client.StatusEvent) (bool, error) {
	t, err := sqlc.New(tx).GetPlanningTurn(ctx, ev.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	blocked, e := resumeDeliveryBlocked(tx, ev.TaskID, true)
	if e != nil {
		return true, e
	}
	if blocked {
		_, e = markResumeRunning(tx, ev.Target, ev.Seq, ev.State)
		return true, e
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return true, err
	}
	if err = sqlc.New(tx).RetainPlanningDelivery(ctx, sqlc.RetainPlanningDeliveryParams{TurnID: t.ID, Kind: "event", Payload: string(data), ReceivedAt: time.Now().UnixMilli()}); err != nil {
		return true, err
	}
	reported, err := acceptedReport(tx, t.ID)
	if err != nil {
		return true, err
	}
	if reported {
		return true, nil
	}
	var interacted bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM planning_interactions WHERE task_id=?)`, t.ID).Scan(&interacted); err != nil {
		return true, err
	}
	if interacted {
		return true, nil
	} // Waiting/running events must not erase a typed interaction or approval.
	// Capture an admission arriving after local cancellation only to cancel its
	// exact remote identity. It must never revive the session or turn.
	if t.State == "cancelled" && t.AgentID == "" && planningIdentityMatches(t, ev.AgentID, ev.RunID, ev.AttemptID) {
		_, err = tx.Exec(`UPDATE planning_turns SET agent_id=?,run_id=?,remote_attempt_id=? WHERE id=? AND state='cancelled' AND agent_id=''`, ev.AgentID, ev.RunID, ev.AttemptID, t.ID)
		return true, err
	}
	current, err := planningTurnCurrent(tx, t)
	if err != nil {
		return true, err
	}
	if !current || t.State == "failed" || !planningIdentityMatches(t, ev.AgentID, ev.RunID, ev.AttemptID) || (t.AgentID != "" && int64(ev.Seq) <= t.EventSeq) {
		return true, nil
	}
	// Status events never carry an authoritative proposal.
	state, session, reason := "running", "running", ""
	switch string(ev.State) {
	case "failed", "cancelled":
		state, session, reason = "failed", "failed", "planner run "+string(ev.State)
	case "waiting", "interrupted":
		reason = "planner is " + string(ev.State) + "; awaiting terminal outcome"
	}
	now := time.Now().UnixMilli()
	_, err = tx.Exec(`UPDATE planning_turns SET agent_id=?,run_id=?,remote_attempt_id=?,event_seq=?,state=?,error=?,updated_at=? WHERE id=?`, ev.AgentID, ev.RunID, ev.AttemptID, ev.Seq, state, reason, now, t.ID)
	if err != nil {
		return true, err
	}
	_, err = tx.Exec(`UPDATE planning_sessions SET state=?,error=?,updated_at=? WHERE id=?`, session, reason, now, t.SessionID)
	return true, err
}
func (w *Workflow) planningResult(tx *sql.Tx, ctx context.Context, out client.Outcome) (bool, error) {
	return w.planningResultDelivery(tx, ctx, out, false)
}
func (w *Workflow) planningResultDelivery(tx *sql.Tx, ctx context.Context, out client.Outcome, trusted bool) (bool, error) {
	q := sqlc.New(tx)
	t, err := q.GetPlanningTurn(ctx, out.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	blocked, e := resumeOutcomeBlocked(tx, out)
	if e != nil {
		return true, e
	}
	if blocked && !trusted && out.State != runtime.RunWaiting {
		return true, nil
	}
	data, err := json.Marshal(out)
	if err != nil {
		return true, err
	}
	if err = q.RetainPlanningDelivery(ctx, sqlc.RetainPlanningDeliveryParams{TurnID: t.ID, Kind: "outcome", Payload: string(data), ReceivedAt: time.Now().UnixMilli()}); err != nil {
		return true, err
	}
	if !planningIdentityMatches(t, out.AgentID, out.RunID, out.AttemptID) || t.State == "cancelled" || t.OutcomeJson != "" {
		return true, nil
	}
	if out.State == runtime.RunWaiting {
		if out.Version != 1 || out.Suspend == nil || out.Error != "" || out.AnswerSubject != "bonnie.answers."+out.AgentID {
			return true, nil
		}
		var request string
		err = tx.QueryRow(`SELECT r.request_json FROM typed_reports r JOIN planning_interactions i ON i.report_id=r.report_id WHERE i.task_id=? AND r.tool_call_id=? AND i.status IN ('pending','queued')`, t.ID, out.Suspend.ToolCallID).Scan(&request)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return true, err
		}
		var r reportprotocol.Request
		if err = json.Unmarshal([]byte(request), &r); err != nil {
			return true, err
		}
		if r.AgentID != out.AgentID || r.RunID != out.RunID || r.AttemptID != out.AttemptID || r.TurnID != out.Suspend.TurnID {
			return true, nil
		}
		_, err = tx.Exec(`UPDATE planning_interactions SET waiting_json=? WHERE report_id=? AND status IN ('pending','queued')`, string(data), r.ReportID)
		return true, err
	}
	var sessionState string
	if err = tx.QueryRow(`SELECT state FROM planning_sessions WHERE id=?`, t.SessionID).Scan(&sessionState); err != nil {
		return true, err
	}
	if sessionState == "approved" {
		// Post-approval completion/failure is planning bookkeeping only, never build authority.
		if out.State == runtime.RunCompleted || out.State == runtime.RunFailed || out.State == runtime.RunCancelled {
			_, err = tx.Exec(`UPDATE planning_turns SET state=?,outcome_json=?,error=?,updated_at=? WHERE id=?`, string(out.State), string(data), out.Error, time.Now().UnixMilli(), t.ID)
		}
		return true, err
	}
	current, err := planningTurnCurrent(tx, t)
	if err != nil || !current {
		return true, err
	}
	reported, err := acceptedReport(tx, t.ID)
	if err != nil || reported {
		return true, err
	}
	// A confirmed typed suspension is newer authoritative work. BONNIE may
	// redeliver an earlier natural completion after a chat continuation; it
	// must not cancel the proposal the same run is now waiting to review.
	if out.State == runtime.RunCompleted {
		var suspended bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM planning_interactions WHERE task_id=? AND status IN ('pending','queued') AND waiting_json<>'')`, t.ID).Scan(&suspended); err != nil || suspended {
			return true, err
		}
	}
	reason := out.Error
	if reason == "" {
		reason = "planner finished without an accepted typed report"
	}
	now := time.Now().UnixMilli()
	if _, err = tx.Exec(`UPDATE planning_turns SET state='failed',agent_id=?,run_id=?,remote_attempt_id=?,result=?,outcome_json=?,error=?,updated_at=? WHERE id=?`, out.AgentID, out.RunID, out.AttemptID, out.Response, string(data), reason, now, t.ID); err != nil {
		return true, err
	}
	if _, err = tx.Exec(`UPDATE planning_interactions SET status='cancelled' WHERE task_id=? AND status IN ('pending','queued')`, t.ID); err != nil {
		return true, err
	}
	if err = planningMessage(tx, t.SessionID, "assistant", reason); err != nil {
		return true, err
	}
	_, err = tx.Exec(`UPDATE planning_sessions SET state='failed',error=?,updated_at=? WHERE id=?`, reason, now, t.SessionID)
	return true, err
}

func (w *Workflow) dispatchPlanning() {
	w.dispatchPlanningCancellations()
	w.dispatchPlanningReplies()
	if w.client == nil {
		return
	}
	turns, err := sqlc.New(w.s.db).ListQueuedPlanningTurns(w.ctx)
	if err != nil {
		return
	}
	for _, t := range turns {
		// Serialize cancellation/approval against publish. The small bounded broker call
		// inside the write lock prevents a cancelled queued task being published later.
		tx, err := w.s.writeTx()
		if err != nil {
			return
		}
		latest, err := sqlc.New(tx).GetPlanningTurn(w.ctx, t.ID)
		if err != nil {
			_ = tx.Rollback()
			continue
		}
		current, err := planningTurnCurrent(tx, latest)
		if err != nil || !current || latest.Published != 0 || latest.State != "queued" {
			_ = tx.Rollback()
			continue
		}
		var task client.Task
		err = json.Unmarshal([]byte(latest.TaskJson), &task)
		if err == nil {
			ctx, cancel := context.WithTimeout(w.ctx, 2*time.Second)
			err = w.submitTask(ctx, latest.TargetAgentID, task)
			cancel()
		}
		now := time.Now().UnixMilli()
		if err != nil {
			_, _ = tx.Exec(`UPDATE planning_turns SET error=?,updated_at=? WHERE id=?`, err.Error(), now, t.ID)
		} else {
			if _, err = tx.Exec(`UPDATE planning_turns SET published=1,state='submitted',error='',updated_at=? WHERE id=?`, now, t.ID); err != nil {
				_ = tx.Rollback()
				continue
			}
		}
		// Publish failures are retained and queued work remains retryable.
		_ = tx.Commit()
	}
}

// Cancellation commands are request/reply, not durable in BONNIE. Local cancelled
// turns therefore form a retryable command queue until an owner acknowledges.
func (w *Workflow) dispatchPlanningCancellations() {
	if w.client == nil {
		return
	}
	q := sqlc.New(w.s.db)
	turns, err := q.ListPlanningCancellations(w.ctx)
	if err != nil {
		return
	}
	for _, t := range turns {
		target := client.Target{TaskID: t.ID, AgentID: t.AgentID, RunID: t.RunID, AttemptID: t.RemoteAttemptID}
		ctx, cancel := context.WithTimeout(w.ctx, time.Second)
		status, err := w.client.Status(ctx, target)
		cancel()
		if err != nil || status.Error != "" || status.Target != target {
			continue
		}
		terminal := status.State == "completed" || status.State == "failed" || status.State == "cancelled"
		if !terminal {
			if status.TurnID == "" {
				continue
			}
			ctx, cancel = context.WithTimeout(w.ctx, time.Second)
			reply, e := w.client.CancelTurn(ctx, target, status.TurnID)
			cancel()
			if e != nil || reply.Error != "" || reply.Target != target || !reply.CancelRequested {
				continue
			}
		}
		_ = q.MarkPlanningCancellationSent(w.ctx, sqlc.MarkPlanningCancellationSentParams{ID: t.ID, AgentID: t.AgentID, RunID: t.RunID, RemoteAttemptID: t.RemoteAttemptID})
	}
}
