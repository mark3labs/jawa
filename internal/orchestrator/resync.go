package orchestrator

import (
	"context"
	"database/sql"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/nats-io/nats.go"
)

// Resync is read-only on agents: it never resumes, cancels, or replaces a run.
// Retry unreachable owners on the next sweep; silence is not a failed task.
func (w *Workflow) resyncRuns(conn *nats.Conn) {
	// Recover final reports before slow/unreachable status owners can exhaust
	// the sweep budget. Result recovery has its own bounded deadline.
	_ = w.RecoverEvents(w.ctx, conn)
	_ = w.RecoverResults(w.ctx, conn)
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	rows, err := w.s.db.QueryContext(ctx, `SELECT `+attemptColumns+` FROM workflow_attempts a WHERE a.card_id IN (SELECT id FROM cards WHERE status='Building') AND a.ready=0 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=a.card_id)`)
	if err != nil {
		return
	}
	var attempts []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			_ = rows.Close()
			return
		}
		attempts = append(attempts, a)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return
	}
	for _, a := range attempts {
		if ctx.Err() != nil {
			return
		}
		if a.AgentID == "" || a.RunID == "" || a.RemoteAttemptID == "" || (a.OutcomeJSON != "" && !resumableOutcome(a)) {
			continue
		}
		target := client.Target{TaskID: a.TaskID, AgentID: a.AgentID, RunID: a.RunID, AttemptID: a.RemoteAttemptID}
		queryCtx, stop := context.WithTimeout(ctx, time.Second)
		status, err := w.client.Status(queryCtx, target)
		stop()
		if err != nil || status.Error != "" || status.Target != target {
			continue
		}
		_ = w.applyStatus(a, status)
	}
}

func (w *Workflow) applyStatus(observed Attempt, status client.Status) error {
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := scanAttempt(tx.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE id=?`, observed.ID))
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if status.Version != 1 || status.Error != "" || status.TaskID != a.TaskID || !matches(a, status.AgentID, status.RunID, status.AttemptID) || a.Ready || status.Seq < a.EventSeq || a.OutcomeJSON != observed.OutcomeJSON || (a.OutcomeJSON != "" && !resumableOutcome(a)) {
		return nil
	}
	var current bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM cards WHERE id=? AND status='Building' AND ?=(SELECT max(number) FROM workflow_attempts WHERE card_id=?))`, a.CardID, a.Number, a.CardID).Scan(&current); err != nil {
		return err
	}
	if !current {
		return nil
	}
	state, reason := "running", ""
	switch string(status.State) {
	case "pending":
		state = "submitted"
	case "running":
		if !status.Active {
			state, reason = "blocked", "agent run interrupted; awaiting agent recovery"
		}
	case "interrupted":
		state, reason = "blocked", "agent run interrupted; awaiting agent recovery"
	case "waiting":
		state, reason = "blocked", "agent run waiting for input"
	case "cancelled":
		state, reason = "failed", "agent run cancelled"
	case "failed":
		state, reason = "failed", "agent run failed; awaiting agent outcome"
	case "completed":
		state, reason = "blocked", "awaiting agent outcome"
	default:
		return nil
	}
	if a.State == state && a.RunState == string(status.State) && a.Error == reason && a.EventSeq == status.Seq {
		return nil
	}
	_, err = tx.Exec(`UPDATE workflow_attempts SET state=?,run_state=?,error=?,event_seq=?,updated_at=? WHERE id=?`, state, string(status.State), reason, status.Seq, time.Now().UnixMilli(), a.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
