package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/orchestrator/db/sqlc"
)

// The durable outbox exists before dispatch. A report ACK can precede BONNIE's
// RecordSuspend/result: do not guess a route or manufacture a waiting outcome.
func (w *Workflow) dispatchPlanningReplies() {
	if w.client == nil {
		return
	}
	ids, err := sqlc.New(w.s.db).ListQueuedPlanningReplies(w.ctx)
	if err != nil {
		return
	}
	for _, id := range ids {
		// Serialize cancellation against publish; failures remain queued across restart.
		tx, err := w.s.writeTx()
		if err != nil {
			return
		}
		var raw, text, task string
		var approved sql.NullBool
		err = tx.QueryRow(`SELECT waiting_json,reply,approved,task_id FROM planning_interactions WHERE report_id=? AND status='queued'`, id).Scan(&raw, &text, &approved, &task)
		var out client.Outcome
		if err == nil {
			err = json.Unmarshal([]byte(raw), &out)
		}
		if err == nil && out.Suspend != nil {
			response := runtime.InputResponse{TurnID: out.Suspend.TurnID, Text: text}
			if approved.Valid {
				response.Approved = &approved.Bool
			}
			ctx, cancel := context.WithTimeout(w.ctx, 2*time.Second)
			_, err = w.client.Answer(ctx, out, []runtime.InputResponse{response})
			cancel()
		} else if err == nil {
			err = sql.ErrNoRows
		}
		if err != nil {
			_ = tx.Rollback()
			continue
		}
		if _, err = tx.Exec(`UPDATE planning_interactions SET status='answered',answered_at=? WHERE report_id=? AND status='queued'`, time.Now().UnixMilli(), id); err == nil {
			_, err = tx.Exec(`UPDATE planning_turns SET state='running',error='',updated_at=? WHERE id=? AND state='waiting'`, time.Now().UnixMilli(), task)
		}
		if err != nil {
			_ = tx.Rollback()
		} else {
			_ = tx.Commit()
		}
	}
}
