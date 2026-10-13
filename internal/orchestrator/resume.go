package orchestrator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
	"jawa/internal/orchestrator/db/sqlc"
	"jawa/internal/reportprotocol"
)

func executionFailure(a Attempt) bool {
	var out client.Outcome
	return a.State == "failed" && !a.Ready && a.RunState == "failed" && a.Error != executionConflictNotice && a.AgentID != "" && a.RunID != "" && a.RemoteAttemptID != "" && json.Unmarshal([]byte(a.OutcomeJSON), &out) == nil && out.Version == 1 && out.State == runtime.RunFailed && out.TaskID == a.TaskID && matches(a, out.AgentID, out.RunID, out.AttemptID)
}

func (w *Workflow) ResumeFailed(id string) error         { return w.resumeFailed(id, false, "") }
func (w *Workflow) ResumeFailedPlanning(id string) error { return w.resumeFailed(id, true, "") }

// Bind rendered actions to this failure and journal generation.
func resumeToken(a Attempt) string {
	return fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "%s\n%d\n%s", a.TaskID, a.EventSeq, a.OutcomeJSON)))
}

// Reserve before sending control. Unconfirmed delivery blocks competing work
// across process restart; it is not permission for a blind fresh task.
func (w *Workflow) resumeFailed(id string, planning bool, expected string) error {
	return w.reserveFailedResume(id, planning, expected, false, time.Now())
}

func (w *Workflow) reserveFailedResume(id string, planning bool, expected string, automatic bool, clock time.Time) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.client == nil {
		return errors.New("agent control unavailable")
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var a Attempt
	var session string
	if planning {
		p, e := currentPlanning(tx, w.ctx, id)
		if e != nil {
			return e
		}
		if p.State != "failed" {
			return errors.New("resume requires failed planning")
		}
		session = p.ID
		if e = noActivePlanningTurns(tx, p.ID); e != nil {
			return e
		}
		var taskID string
		e = tx.QueryRow(`SELECT id FROM planning_turns WHERE session_id=? ORDER BY number DESC LIMIT 1`, p.ID).Scan(&taskID)
		if e != nil {
			return e
		}
		t, e := sqlc.New(tx).GetPlanningTurn(w.ctx, taskID)
		if e != nil {
			return e
		}
		a = Attempt{ID: t.ID, TaskID: t.ID, State: t.State, RunState: "failed", OutcomeJSON: t.OutcomeJson, AgentID: t.AgentID, RunID: t.RunID, RemoteAttemptID: t.RemoteAttemptID, Error: t.Error, EventSeq: int(t.EventSeq)}
	} else {
		a, err = scanAttempt(tx.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE card_id=? ORDER BY number DESC LIMIT 1`, id))
		if err != nil {
			return err
		}
		var lane string
		if err = tx.QueryRow(`SELECT status FROM cards WHERE id=?`, id).Scan(&lane); err != nil {
			return err
		}
		if lane != "Building" && lane != "Fix" {
			return errors.New("resume requires Building or Fix")
		}
		if err = noActiveAttempts(tx, id); err != nil {
			return err
		}
	}
	if expected != "" && expected != resumeToken(a) {
		return errors.New("failed run changed; refresh before resuming")
	}
	if !executionFailure(a) {
		return errors.New("resume requires an owned execution failure; use fresh Retry when safe")
	}
	reported, err := acceptedReport(tx, a.TaskID)
	if err != nil {
		return err
	}
	if reported {
		return errors.New("terminal report cannot be resumed")
	}
	// Any retained competing delivery makes ownership ambiguous, even if the
	// pinned execution itself failed.
	var conflict bool
	if planning {
		err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM planning_deliveries WHERE turn_id=? AND (json_extract(payload,'$.agent_id')<>? OR json_extract(payload,'$.run_id')<>? OR json_extract(payload,'$.attempt_id')<>?))`, a.TaskID, a.AgentID, a.RunID, a.RemoteAttemptID).Scan(&conflict)
	} else {
		err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workflow_conflicts WHERE task_id=?)`, a.TaskID).Scan(&conflict)
	}
	if err != nil {
		return err
	}
	if conflict {
		return errors.New("ambiguous execution ownership; reconcile before retrying")
	}
	if automatic {
		enabled, e := autoResumeEnabled(tx)
		if e != nil {
			return e
		}
		if !enabled {
			return errors.New("automatic resume disabled")
		}
		var token, state string
		var due, count int64
		if e = tx.QueryRow(`SELECT failure_token,state,due_at FROM auto_resumes WHERE task_id=?`, a.TaskID).Scan(&token, &state, &due); e != nil {
			return e
		}
		if e = tx.QueryRow(`SELECT count(*) FROM failed_resumes WHERE task_id=? AND automatic=1`, a.TaskID).Scan(&count); e != nil {
			return e
		}
		if state != "scheduled" || token != resumeToken(a) || due > clock.UnixMilli() || count >= 5 {
			return errors.New("automatic schedule changed or not due")
		}
	}
	target := client.Target{TaskID: a.TaskID, AgentID: a.AgentID, RunID: a.RunID, AttemptID: a.RemoteAttemptID}
	ctx, cancel := context.WithTimeout(w.ctx, time.Second)
	defer cancel()
	status, err := w.client.Status(ctx, target)
	if err != nil || status.Error != "" || status.Target != target || status.State != runtime.RunFailed || status.Active || status.TurnID == "" || status.Seq < a.EventSeq {
		return errors.New("failed run cannot be confirmed on its owning agent; fresh Retry remains available")
	}
	req := reportprotocol.FailedResume{Version: 1, ID: uuid.NewString(), Target: target, Seq: status.Seq, TurnID: status.TurnID}
	raw, _ := json.Marshal(req)
	now := clock.UnixMilli()
	if _, err = tx.Exec(`INSERT INTO failed_resumes(id,task_id,card_id,planning,request_json,previous_outcome,created_at,automatic) VALUES(?,?,?,?,?,?,?,?)`, req.ID, a.TaskID, id, planning, raw, a.OutcomeJSON, now, automatic); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE auto_resumes SET state='reserved' WHERE task_id=?`, a.TaskID); err != nil {
		return err
	}
	if planning {
		_, err = tx.Exec(`UPDATE planning_turns SET state='running',outcome_json='',error='Resume pending confirmation',event_seq=?,updated_at=? WHERE id=?`, status.Seq, now, a.TaskID)
		if err == nil {
			_, err = tx.Exec(`UPDATE planning_sessions SET state='running',error='Resume pending confirmation',updated_at=? WHERE id=?`, now, session)
		}
	} else {
		_, err = tx.Exec(`UPDATE workflow_attempts SET state='running',run_state='running',outcome_json='',error='Resume pending confirmation',event_seq=?,updated_at=? WHERE id=?`, status.Seq, now, a.ID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

const resumePendingNotice = "Resume pending confirmation"

// markResumeRunning clears the pending notice once the exact resumed run is
// observed executing past the reserved journal cursor, so the UI shows normal
// work instead of "Resuming…". The failed_resumes row stays pending and keeps
// blocking competing deliveries until the durable host receipt arrives.
func markResumeRunning(tx *sql.Tx, target client.Target, seq int, state runtime.RunState) (bool, error) {
	if state != runtime.RunRunning {
		return false, nil
	}
	var raw string
	var planning bool
	err := tx.QueryRow(`SELECT request_json,planning FROM failed_resumes WHERE task_id=? AND status='pending'`, target.TaskID).Scan(&raw, &planning)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var req reportprotocol.FailedResume
	if err = json.Unmarshal([]byte(raw), &req); err != nil || req.Target != target || seq <= req.Seq {
		return false, nil
	}
	now := time.Now().UnixMilli()
	var res sql.Result
	if planning {
		if res, err = tx.Exec(`UPDATE planning_turns SET error='',updated_at=? WHERE id=? AND state='running' AND error=?`, now, target.TaskID, resumePendingNotice); err != nil {
			return false, err
		}
		if _, err = tx.Exec(`UPDATE planning_sessions SET error='',updated_at=? WHERE id=(SELECT session_id FROM planning_turns WHERE id=?) AND state='running' AND error=?`, now, target.TaskID, resumePendingNotice); err != nil {
			return false, err
		}
	} else if res, err = tx.Exec(`UPDATE workflow_attempts SET error='',updated_at=? WHERE task_id=? AND state='running' AND error=?`, now, target.TaskID, resumePendingNotice); err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// promoteResumeIfRunning clears the pending notice of a resume whose
// continuation is confirmed executing. It reuses an already observed status
// snapshot and otherwise queries the owner only while the notice is shown.
func (w *Workflow) promoteResumeIfRunning(target client.Target, observed *client.Status) {
	if observed == nil {
		if w.client == nil {
			return
		}
		var shown bool
		if err := w.s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM workflow_attempts WHERE task_id=? AND error=?) OR EXISTS(SELECT 1 FROM planning_turns WHERE id=? AND error=?)`, target.TaskID, resumePendingNotice, target.TaskID, resumePendingNotice).Scan(&shown); err != nil || !shown {
			return
		}
		now := time.Now()
		if next, ok := w.resumePromoteChecks[target.TaskID]; ok && now.Before(next) {
			return
		}
		if w.resumePromoteChecks == nil {
			w.resumePromoteChecks = map[string]time.Time{}
		}
		w.resumePromoteChecks[target.TaskID] = now.Add(5 * time.Second)
		ctx, cancel := context.WithTimeout(w.ctx, time.Second)
		status, err := w.client.Status(ctx, target)
		cancel()
		if err != nil {
			return
		}
		observed = &status
	}
	status := *observed
	if status.Error != "" || status.Target != target || !status.Active {
		return
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	if ok, e := markResumeRunning(tx, target, status.Seq, status.State); e == nil && ok {
		if tx.Commit() == nil {
			w.log().Info("Resumed run confirmed executing", "task_id", target.TaskID, "seq", status.Seq)
		}
	}
}

func resumeStaleDuration() (time.Duration, error) {
	value := os.Getenv("JAWA_RESUME_STALE_AFTER")
	if value == "" {
		return 5 * time.Minute, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, errors.New("JAWA_RESUME_STALE_AFTER must be a positive duration")
	}
	return d, nil
}

// Reuse the durable command identity. Expiration requests a host-side fence,
// never a local abandonment that could race a delayed original command.
func (w *Workflow) dispatchFailedResumes(conn *nats.Conn) {
	rows, err := w.s.db.Query(`SELECT id,request_json,created_at FROM failed_resumes WHERE status='pending'`)
	if err != nil {
		w.log().Error("Failed to load pending resumes")
		return
	}
	type command struct {
		id, raw string
		created int64
	}
	var commands []command
	for rows.Next() {
		var c command
		if err = rows.Scan(&c.id, &c.raw, &c.created); err != nil {
			break
		}
		commands = append(commands, c)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		w.log().Error("Failed to read pending resumes")
		return
	}
	threshold := w.resumeStaleAfter
	if threshold <= 0 {
		threshold = 5 * time.Minute
	}
	// Forget schedules for commands that are no longer pending.
	pending := make(map[string]bool, len(commands))
	for _, c := range commands {
		pending[c.id] = true
	}
	for id := range w.resumeChecks {
		if !pending[id] {
			delete(w.resumeChecks, id)
		}
	}
	pendingTasks := make(map[string]bool, len(commands))
	for _, c := range commands {
		var req reportprotocol.FailedResume
		if json.Unmarshal([]byte(c.raw), &req) == nil {
			pendingTasks[req.Target.TaskID] = true
		}
	}
	for task := range w.resumePromoteChecks {
		if !pendingTasks[task] {
			delete(w.resumePromoteChecks, task)
		}
	}
	for _, c := range commands {
		var req reportprotocol.FailedResume
		if json.Unmarshal([]byte(c.raw), &req) != nil {
			w.log().Error("Invalid pending resume command", "resume_id", c.id)
			continue
		}
		reconcile, observed := w.resumeNeedsReconcile(c.id, req.Target, time.UnixMilli(c.created), threshold)
		req.Reconcile = reconcile
		ctx, cancel := context.WithTimeout(w.ctx, time.Second)
		reply, e := reportprotocol.ResumeFailed(ctx, conn, "bonnie", req)
		cancel()
		if e != nil {
			w.log().Warn("Resume delivery failed; will retry", "resume_id", c.id)
			continue
		}
		if reply.Status == "pending" {
			w.promoteResumeIfRunning(req.Target, observed)
			continue
		}
		if e = w.finishFailedResume(c.id, reply); e != nil {
			w.log().Error("Failed to apply resume receipt", "resume_id", c.id, "status", reply.Status)
		}
	}
}

// Staleness is measured from the last observed progress, not reservation age:
// an active owner counts as progress and defers the next check by a full
// threshold. Unconfirmed owners are re-queried at a bounded rate. Only a
// confirmed idle failed run requests the host-side fence. A confirmed status
// snapshot is returned when one was queried, so callers need not re-query.
func (w *Workflow) resumeNeedsReconcile(id string, target client.Target, created time.Time, threshold time.Duration) (bool, *client.Status) {
	now := time.Now()
	if now.Sub(created) < threshold {
		return false, nil
	}
	if next, ok := w.resumeChecks[id]; ok && now.Before(next) {
		return false, nil
	}
	if w.resumeChecks == nil {
		w.resumeChecks = map[string]time.Time{}
	}
	retry := min(threshold, 30*time.Second)
	if w.client == nil {
		w.resumeChecks[id] = now.Add(retry)
		w.log().Warn("Resume remains pending; owning agent unavailable", "resume_id", id, "task_id", target.TaskID)
		return false, nil
	}
	ctx, cancel := context.WithTimeout(w.ctx, time.Second)
	status, e := w.client.Status(ctx, target)
	cancel()
	confirmed := e == nil && status.Error == "" && status.Target == target
	if e != nil {
		// Still report the query so callers do not re-query this tick.
		status.Error = e.Error()
	}
	switch {
	case confirmed && status.Active:
		w.resumeChecks[id] = now.Add(threshold)
		w.log().Debug("Resume pending; owning agent still running", "resume_id", id, "task_id", target.TaskID, "seq", status.Seq)
		return false, &status
	case confirmed && status.State == runtime.RunFailed:
		// If the fence is not yet durable the next attempt re-confirms first.
		w.resumeChecks[id] = now.Add(retry)
		w.log().Warn("Resume stalled on idle failed run; reconciling", "resume_id", id, "task_id", target.TaskID)
		return true, &status
	default:
		// Absence and transport failures are not proof of safe abandonment.
		w.resumeChecks[id] = now.Add(retry)
		w.log().Warn("Resume remains pending; owning agent unconfirmed", "resume_id", id, "task_id", target.TaskID, "state", status.State)
		return false, &status
	}
}

func (w *Workflow) finishFailedResume(id string, reply reportprotocol.FailedResumeReply) error {
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var task, previous, requestJSON string
	var planning, automatic bool
	var created int64
	err = tx.QueryRow(`SELECT task_id,previous_outcome,planning,request_json,automatic,created_at FROM failed_resumes WHERE id=? AND status='pending'`, id).Scan(&task, &previous, &planning, &requestJSON, &automatic, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var req reportprotocol.FailedResume
	if err = json.Unmarshal([]byte(requestJSON), &req); err != nil {
		return err
	}
	if reply.Version != 1 || reply.ID != id || reply.Target != req.Target ||
		(reply.Status != "completed" && reply.Status != "rejected" && reply.Status != "ambiguous") {
		return errors.New("invalid resume receipt identity or status")
	}
	if reply.Status == "completed" && (reply.Seq <= req.Seq || reply.Outcome == nil ||
		reply.Outcome.Version != 1 || reply.Outcome.TaskID != req.Target.TaskID ||
		reply.Outcome.AgentID != req.Target.AgentID || reply.Outcome.RunID != req.Target.RunID ||
		reply.Outcome.AttemptID != req.Target.AttemptID) {
		return errors.New("invalid resumed outcome or journal cursor")
	}
	raw, _ := json.Marshal(reply)
	if _, err = tx.Exec(`UPDATE failed_resumes SET status=?,reply_json=? WHERE id=?`, reply.Status, raw, id); err != nil {
		return err
	}
	// A late non-completed receipt must not roll back progress the resumed run
	// already reported through typed tools (for example a verified ready PR).
	superseded := false
	if reply.Status != "completed" {
		if superseded, err = resumeSuperseded(tx, task, created); err != nil {
			return err
		}
	}
	if superseded {
		w.log().Warn("Ignoring late resume receipt; typed report already accepted", "resume_id", id, "task_id", task, "status", reply.Status)
	} else if reply.Status != "completed" {
		notice := "Resume rejected: " + reply.Error
		if reply.Status == "ambiguous" {
			notice = "Resume could not be confirmed: " + reply.Error
		}
		if planning {
			_, err = tx.Exec(`UPDATE planning_turns SET state='failed',outcome_json=?,error=?,updated_at=? WHERE id=?`, previous, notice, time.Now().UnixMilli(), task)
			if err == nil {
				_, err = tx.Exec(`UPDATE planning_sessions SET state='failed',error=? WHERE id=(SELECT session_id FROM planning_turns WHERE id=?)`, notice, task)
			}
		} else {
			_, err = tx.Exec(`UPDATE workflow_attempts SET state='failed',run_state='failed',outcome_json=?,error=?,updated_at=? WHERE task_id=?`, previous, notice, time.Now().UnixMilli(), task)
		}
	} else if reply.Outcome != nil && reply.Outcome.TaskID == task && reply.Outcome.AgentID == reply.Target.AgentID && reply.Outcome.RunID == reply.Target.RunID && reply.Outcome.AttemptID == reply.Target.AttemptID {
		// New outcomes come only from the durable host receipt, never a replay of
		// the pre-resume transport cache. Accepted typed reports still win.
		if planning {
			_, err = w.planningResultDelivery(tx, w.ctx, *reply.Outcome, true)
		} else {
			var a Attempt
			a, err = scanAttempt(tx.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE task_id=?`, task))
			if err == nil {
				err = applyOutcome(tx, a, *reply.Outcome)
			}
		}
		if err == nil {
			table := "workflow_attempts"
			if planning {
				table = "planning_turns"
			}
			_, err = tx.Exec(`UPDATE `+table+` SET event_seq=max(event_seq,?) WHERE id=?`, reply.Seq, task)
		}
	} else {
		return errors.New("invalid resumed outcome")
	}
	if err != nil {
		return err
	}
	if reply.Status == "completed" || superseded {
		if _, err = tx.Exec(`DELETE FROM auto_resumes WHERE task_id=? AND state='reserved'`, task); err != nil {
			return err
		}
	}
	if automatic && reply.Status != "completed" && !superseded {
		var a Attempt
		if planning {
			t, e := sqlc.New(tx).GetPlanningTurn(w.ctx, task)
			if e != nil {
				return e
			}
			a = Attempt{TaskID: t.ID, EventSeq: int(t.EventSeq), OutcomeJSON: t.OutcomeJson}
		} else {
			a, err = scanAttempt(tx.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE task_id=?`, task))
			if err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`UPDATE auto_resumes SET state='stopped',failure_token=? WHERE task_id=?`, resumeToken(a), task); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// resumeSuperseded reports whether the task already has an accepted terminal
// typed report, or any typed report accepted after the resume was requested.
// Either proves the continuation ran, so its previous failure is stale.
func resumeSuperseded(tx *sql.Tx, task string, created int64) (bool, error) {
	var found bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM typed_reports WHERE task_id=? AND (kind IN ('complete_task','report_blocker') OR accepted_at>=?))`, task, created).Scan(&found)
	return found, err
}

// BONNIE's cached pre-resume outcome lacks a journal cursor. Once a JAWA
// continuation exists, terminal natural outcomes are read from host receipts
// instead. Waiting answers remain on the normal planning transport path.
func resumeDeliveryBlocked(tx *sql.Tx, task string, event bool) (bool, error) {
	var blocked bool
	query := `SELECT EXISTS(SELECT 1 FROM failed_resumes WHERE task_id=? )`
	if event {
		query = `SELECT EXISTS(SELECT 1 FROM failed_resumes WHERE task_id=? AND status='pending')`
	}
	err := tx.QueryRow(query, task).Scan(&blocked)
	return blocked, err
}

func resumeOutcomeBlocked(tx *sql.Tx, out client.Outcome) (bool, error) {
	rows, err := tx.Query(`SELECT status,previous_outcome FROM failed_resumes WHERE task_id=?`, out.TaskID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status, raw string
		if err = rows.Scan(&status, &raw); err != nil {
			return false, err
		}
		var previous client.Outcome
		if status == "pending" {
			return true, nil
		}
		if json.Unmarshal([]byte(raw), &previous) == nil {
			a, _ := json.Marshal(previous)
			b, _ := json.Marshal(out)
			if string(a) == string(b) {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}
func noPendingResume(tx *sql.Tx, card string) error {
	var pending bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM failed_resumes WHERE card_id=? AND status='pending')`, card).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return errors.New("resume is pending confirmation; wait for its owning agent")
	}
	return nil
}
