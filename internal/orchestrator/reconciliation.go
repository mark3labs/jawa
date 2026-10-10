package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/log"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/nats-io/nats.go"
)

func retainConflict(tx *sql.Tx, out client.Outcome) error {
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	// The caller holds SQLite's write lock. Keep the first receipt of each
	// exact outcome; replaying broker history must not create new evidence.
	_, err = tx.Exec(`INSERT INTO workflow_conflicts(task_id,agent_id,run_id,remote_attempt_id,outcome_json,received_at,reason)
 SELECT ?,?,?,?,?,?,? WHERE NOT EXISTS (
 SELECT 1 FROM workflow_conflicts WHERE task_id=? AND agent_id=? AND run_id=? AND remote_attempt_id=? AND outcome_json=?)`,
		out.TaskID, out.AgentID, out.RunID, out.AttemptID, string(data), time.Now().UnixMilli(), "execution identity differs from pinned attempt",
		out.TaskID, out.AgentID, out.RunID, out.AttemptID, string(data))
	return err
}

// Reconcile explicitly selects a retained terminal execution. Selection, identity
// replacement, application and audit all commit together; delivery cannot race
// through an unlocked interval. This never grants PR readiness.
func (w *Workflow) Reconcile(taskID, runID, remoteAttemptID string) (retErr error) {
	w.log().Info("workflow reconciliation requested", "task_id", taskID, "run_id", runID, "remote_attempt_id", remoteAttemptID)
	defer func() {
		if retErr != nil {
			w.log().Warn("workflow reconciliation failed", "task_id", taskID, "run_id", runID, "remote_attempt_id", remoteAttemptID)
		}
	}()
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if taskID == "" || runID == "" || remoteAttemptID == "" {
		return errors.New("reconcile: nonempty task, run and attempt identities required")
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := scanAttempt(tx.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE task_id=?`, taskID))
	if err != nil {
		return err
	}
	var status string
	var latest int
	if err = tx.QueryRow(`SELECT status FROM cards WHERE id=?`, a.CardID).Scan(&status); err != nil {
		return err
	}
	if err = tx.QueryRow(`SELECT max(number) FROM workflow_attempts WHERE card_id=?`, a.CardID).Scan(&latest); err != nil {
		return err
	}
	if a.Ready || status != "Building" || latest != a.Number {
		return errors.New("reconcile: requires latest unready attempt on a Building card")
	}
	// Select the most recently received terminal report for this execution. A
	// waiting report does not hide its later completion. Agent ambiguity is
	// rejected because the public selector cannot distinguish those executions.
	rows, err := tx.Query(`SELECT id,outcome_json FROM workflow_conflicts WHERE task_id=? AND run_id=? AND remote_attempt_id=? ORDER BY id`, taskID, runID, remoteAttemptID)
	if err != nil {
		return err
	}
	var conflictID int64
	var selected string
	var selectedAgent string
	for rows.Next() {
		var id int64
		var raw string
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var candidate client.Outcome
		if err = json.Unmarshal([]byte(raw), &candidate); err != nil {
			break
		}
		if string(candidate.State) != "completed" && string(candidate.State) != "failed" {
			continue
		}
		if selected != "" && selectedAgent != candidate.AgentID {
			w.logAttempt(log.WarnLevel, "workflow reconciliation rejected ambiguous retained agents", a)
			err = errors.New("reconcile: ambiguous retained agents")
			break
		}
		conflictID, selected, selectedAgent = id, raw, candidate.AgentID
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if selected == "" {
		return errors.New("reconcile: no retained conflicting outcome")
	}
	var out client.Outcome
	if err = json.Unmarshal([]byte(selected), &out); err != nil {
		return err
	}
	if out.TaskID != taskID || out.RunID != runID || out.AttemptID != remoteAttemptID || out.AgentID == "" || (string(out.State) != "completed" && string(out.State) != "failed") {
		return errors.New("reconcile: retained outcome must be completed or failed with matching nonempty identities")
	}
	if matches(a, out.AgentID, out.RunID, out.AttemptID) {
		return errors.New("reconcile: selected execution is already pinned")
	}
	now := time.Now().UnixMilli()
	if _, err = tx.Exec(`UPDATE workflow_attempts SET agent_id=?,run_id=?,remote_attempt_id=?,outcome_json='',run_state='',event_seq=-1,result='',error='',pr_url='' WHERE id=?`, out.AgentID, out.RunID, out.AttemptID, a.ID); err != nil {
		return err
	}
	if err = applyOutcome(tx, a, out); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO workflow_reconciliations(task_id,previous_agent_id,previous_run_id,previous_remote_attempt_id,selected_agent_id,selected_run_id,selected_remote_attempt_id,conflict_id,reconciled_at) VALUES(?,?,?,?,?,?,?,?,?)`, taskID, a.AgentID, a.RunID, a.RemoteAttemptID, out.AgentID, out.RunID, out.AttemptID, conflictID, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	w.log().Info("workflow execution conflict reconciled", "attempt_id", a.ID, "card_id", a.CardID,
		"task_id", taskID, "previous_agent_id", a.AgentID, "previous_run_id", a.RunID,
		"previous_remote_attempt_id", a.RemoteAttemptID, "agent_id", out.AgentID,
		"run_id", out.RunID, "remote_attempt_id", out.AttemptID, "run_state", string(out.State), "conflict_id", conflictID)
	return nil
}

// RecoverResults reads retained stream history without consuming, acknowledging,
// deleting messages or changing consumer state. It can recover previously acked
// deliveries after migration and is also safe to invoke manually. The last 10,000
// sequence positions (including holes) and a five-second budget bound the scan.
func (w *Workflow) RecoverResults(ctx context.Context, conn *nats.Conn) (retErr error) {
	var taskCount, scanned, recovered int
	defer func() {
		if retErr != nil {
			w.log().Warn("workflow result recovery failed", "category", "recovery",
				"tasks", taskCount, "scanned", scanned, "replayed", recovered)
		} else {
			w.log().Debug("workflow result recovery summary", "tasks", taskCount,
				"scanned", scanned, "replayed", recovered)
		}
	}()
	if ctx == nil || conn == nil {
		return errors.New("recover results: context and connection required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := w.s.db.QueryContext(ctx, `SELECT a.task_id FROM workflow_attempts a JOIN cards c ON c.id=a.card_id WHERE c.status='Building' AND a.ready=0 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=a.card_id)`)
	if err != nil {
		return err
	}
	tasks := make(map[string]bool)
	for rows.Next() {
		var task string
		if err = rows.Scan(&task); err != nil {
			break
		}
		tasks[task] = true
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	taskCount = len(tasks)
	if len(tasks) == 0 {
		return nil
	}
	js, err := conn.JetStream()
	if err != nil {
		return err
	}
	stream := client.DefaultResultStreamName("bonnie.results")
	info, err := js.StreamInfo(stream, nats.Context(ctx))
	if errors.Is(err, nats.ErrStreamNotFound) {
		return nil
	}
	if err != nil {
		return w.recoveryError(ctx, err)
	}
	first, last := info.State.FirstSeq, info.State.LastSeq
	if info.State.Msgs == 0 {
		return nil
	}
	if last-first >= 10000 {
		first = last - 9999
		w.log().Warn("workflow result recovery history truncated", "sequence_limit", 10000, "first_sequence", first, "last_sequence", last)
	}
	for seq := first; seq <= last; seq++ {
		if err = ctx.Err(); err != nil {
			return w.recoveryError(ctx, err)
		}
		scanned++
		msg, getErr := js.GetMsg(stream, seq, nats.Context(ctx))
		if errors.Is(getErr, nats.ErrMsgNotFound) {
			continue
		}
		if getErr != nil {
			return w.recoveryError(ctx, getErr)
		}
		if msg.Subject != "bonnie.results" {
			continue
		}
		var out client.Outcome
		if json.Unmarshal(msg.Data, &out) != nil || !tasks[out.TaskID] {
			continue
		}
		if err = w.result(ctx, out); err != nil {
			return fmt.Errorf("recover results: %w", err)
		}
		recovered++
		if seq == last {
			break
		} // Avoid uint64 overflow.
	}
	return nil
}

func (w *Workflow) recoveryError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		w.log().Warn("workflow result recovery scan budget exhausted; remaining history skipped", "category", "deadline", "budget_seconds", 5)
		return nil
	}
	return fmt.Errorf("recover results: %w", err)
}
