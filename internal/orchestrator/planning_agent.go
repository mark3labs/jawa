package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"jawa/internal/orchestrator/db/sqlc"
)

var planningCommit = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

type planningReport struct {
	Spec       string   `json:"spec"`
	Questions  []string `json:"questions"`
	RepoCommit string   `json:"repo_commit"`
}

func parsePlanningReport(text string) (planningReport, error) {
	var r planningReport
	if len(text) > 256000 {
		return r, errors.New("planning output exceeds 256000 bytes")
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, errors.New("planner must return the specified JSON object")
	}
	if dec.Decode(new(any)) != io.EOF {
		return r, errors.New("unexpected content after planning report")
	}
	r.Spec = strings.TrimSpace(r.Spec)
	r.RepoCommit = strings.ToLower(strings.TrimSpace(r.RepoCommit))
	if r.Spec == "" || !planningCommit.MatchString(r.RepoCommit) {
		return r, errors.New("planning report requires a spec and full inspected commit SHA")
	}
	if len(r.Questions) > 30 {
		return r, errors.New("too many planning questions")
	}
	for i, v := range r.Questions {
		r.Questions[i] = strings.TrimSpace(v)
		if r.Questions[i] == "" || len(v) > 4000 {
			return r, errors.New("invalid planning question")
		}
	}
	if r.Questions == nil {
		r.Questions = []string{}
	}
	return r, nil
}
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
	// A completed event does not finalize the turn: outcomes contain the specification.
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
	p, err := q.GetPlanningSessionByID(ctx, t.SessionID)
	if err != nil {
		return true, err
	}
	now := time.Now().UnixMilli()
	state, session, reason := "completed", "ready", out.Error
	var report planningReport
	if out.Error != "" || string(out.State) != "completed" {
		state, session = "failed", "failed"
		if reason == "" {
			reason = "planner did not complete; retry investigation (native waiting is not supported by planner contract)"
		}
	} else if out.AgentID == "" || out.RunID == "" || out.AttemptID == "" {
		state, session, reason = "failed", "failed", "planning result lacks execution identity"
	} else {
		report, err = parsePlanningReport(out.Response)
		if err != nil {
			state, session, reason = "failed", "failed", err.Error()
		} else if p.RepoCommit != "" && p.RepoCommit != report.RepoCommit {
			state, session, reason = "failed", "failed", "planner inspected a different commit than the pinned session"
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return true, err
	}
	if _, err = tx.Exec(`UPDATE planning_turns SET state=?,agent_id=?,run_id=?,remote_attempt_id=?,result=?,outcome_json=?,error=?,updated_at=? WHERE id=?`, state, out.AgentID, out.RunID, out.AttemptID, out.Response, string(raw), reason, now, t.ID); err != nil {
		return true, err
	}
	message := out.Response
	if strings.TrimSpace(message) == "" && reason != "" {
		message = reason
	}
	if err = planningMessage(tx, p.ID, "assistant", message); err != nil {
		return true, err
	}
	if state == "completed" {
		questions, err := json.Marshal(report.Questions)
		if err != nil {
			return true, err
		}
		// Revision tokens are card-global, not session-local: an approval form
		// from a cancelled session must not approve revision 1 of a new session.
		var revision int64
		if err = tx.QueryRow(`SELECT coalesce(max(r.number),0)+1 FROM planning_revisions r JOIN planning_sessions s ON s.id=r.session_id WHERE s.card_id=?`, p.CardID).Scan(&revision); err != nil {
			return true, err
		}
		if _, err = tx.Exec(`INSERT INTO planning_revisions(session_id,number,spec,questions_json,repo_commit,created_at) VALUES(?,?,?,?,?,?)`, p.ID, revision, report.Spec, string(questions), report.RepoCommit, now); err != nil {
			return true, err
		}
		if len(report.Questions) > 0 {
			session = "questions"
		}
		_, err = tx.Exec(`UPDATE planning_sessions SET state=?,spec=?,repo_commit=?,revision=?,error='',updated_at=? WHERE id=?`, session, report.Spec, report.RepoCommit, revision, now, p.ID)
		return true, err
	}
	_, err = tx.Exec(`UPDATE planning_sessions SET state=?,error=?,updated_at=? WHERE id=?`, session, reason, now, p.ID)
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
			tx.Rollback()
			continue
		}
		current, err := planningTurnCurrent(tx, latest)
		if err != nil || !current || latest.Published != 0 || latest.State != "queued" {
			tx.Rollback()
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
			_, err = tx.Exec(`UPDATE planning_turns SET published=1,state='submitted',error='',updated_at=? WHERE id=?`, now, t.ID)
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
