package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"jawa/internal/orchestrator/db/sqlc"
)

func planningIdentityMatches(t sqlc.PlanningTurn, agent, run, attempt string) bool {
	return (t.AgentID == "" && t.RunID == "" && t.RemoteAttemptID == "") || (t.AgentID == agent && t.RunID == run && t.RemoteAttemptID == attempt)
}
func planningTurnCurrent(tx *sql.Tx, t sqlc.PlanningTurn) (bool, error) {
	var current bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM planning_sessions s JOIN cards c ON c.id=s.card_id WHERE s.id=? AND c.status='Planning' AND s.state IN ('queued','running','failed') AND ?=(SELECT max(number) FROM planning_turns WHERE session_id=s.id))`, t.SessionID, t.Number).Scan(&current)
	return current && (planningTurnActive(t.State) || (t.State == "failed" && t.OutcomeJson == "")), err
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
	// Capture an admission arriving after local cancellation only to cancel its
	// exact remote identity. It must never revive the session or turn.
	if t.State == "cancelled" && t.AgentID == "" {
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
	q := sqlc.New(tx)
	t, err := q.GetPlanningTurn(ctx, out.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	data, err := json.Marshal(out)
	if err != nil {
		return true, err
	}
	if err = q.RetainPlanningDelivery(ctx, sqlc.RetainPlanningDeliveryParams{TurnID: t.ID, Kind: "outcome", Payload: string(data), ReceivedAt: time.Now().UnixMilli()}); err != nil {
		return true, err
	}
	current, err := planningTurnCurrent(tx, t)
	if err != nil {
		return true, err
	}
	if !current || t.OutcomeJson != "" || !planningIdentityMatches(t, out.AgentID, out.RunID, out.AttemptID) {
		return true, nil
	}
	reported, err := acceptedReport(tx, t.ID)
	if err != nil {
		return true, err
	}
	if reported {
		return true, nil
	}
	reason := out.Error
	if reason == "" {
		reason = "planner finished without an accepted typed report"
	}
	now := time.Now().UnixMilli()
	if _, err = tx.Exec(`UPDATE planning_turns SET state='failed',agent_id=?,run_id=?,remote_attempt_id=?,result=?,outcome_json=?,error=?,updated_at=? WHERE id=?`, out.AgentID, out.RunID, out.AttemptID, out.Response, string(data), reason, now, t.ID); err != nil {
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
			_, err = w.client.Submit(ctx, task)
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
