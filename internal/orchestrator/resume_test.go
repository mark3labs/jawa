package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
	"jawa/internal/reportprotocol"
)

func TestFailedResumeEligibility(t *testing.T) {
	t.Parallel()
	out := client.Outcome{Version: 1, TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed, Error: "connection reset"}
	raw, _ := json.Marshal(out)
	valid := Attempt{CardID: "card", TaskID: "task", AgentID: "agent", RunID: "run", RemoteAttemptID: "remote", State: "failed", RunState: "failed", OutcomeJSON: string(raw)}
	if !executionFailure(valid) {
		t.Fatal("execution failure not resumable")
	}
	for name, edit := range map[string]func(*Attempt){
		"blocker":       func(a *Attempt) { a.State = "blocked"; a.OutcomeJSON = `{"kind":"report_blocker"}` },
		"completed":     func(a *Attempt) { a.RunState = "completed" },
		"cancelled":     func(a *Attempt) { a.RunState = "cancelled" },
		"waiting":       func(a *Attempt) { a.RunState = "waiting" },
		"interrupted":   func(a *Attempt) { a.RunState = "interrupted" },
		"missing owner": func(a *Attempt) { a.AgentID = "" },
		"wrong owner":   func(a *Attempt) { a.RunID = "other" },
		"ambiguous":     func(a *Attempt) { a.Error = executionConflictNotice },
		"ready":         func(a *Attempt) { a.Ready = true },
		"event only":    func(a *Attempt) { a.OutcomeJSON = "" },
	} {
		t.Run(name, func(t *testing.T) {
			a := valid
			edit(&a)
			if executionFailure(a) {
				t.Fatal("unsafe resume offered")
			}
			for _, lane := range []string{"Building", "Fix"} {
				if canAction(Card{ID: "card", Status: lane}, []Attempt{a}, "resume") {
					t.Fatal("unsafe UI action", lane)
				}
			}
		})
	}
	valid.CardID = "card"
	if !canAction(Card{ID: "card", Status: "Building"}, []Attempt{valid}, "resume") || !canAction(Card{ID: "card", Status: "Building"}, []Attempt{valid}, "retry") {
		t.Fatal("resume and fresh Retry must be distinct available actions")
	}
	for _, lane := range []string{"Building", "Fix", "Planning", "Todo", "Done"} {
		card := Card{ID: "card", Status: lane}
		if got, want := canAction(card, []Attempt{valid}, "resume"), lane == "Building" || lane == "Fix"; got != want {
			t.Errorf("%s eligibility=%v", lane, got)
		}
		active := valid
		active.Number = 2
		active.State = "running"
		if canAction(card, []Attempt{valid, active}, "resume") {
			t.Fatal("active attempt allowed", lane)
		}
		newer := valid
		newer.Number = 2
		newer.OutcomeJSON = ""
		if canAction(card, []Attempt{valid, newer}, "resume") {
			t.Fatal("old failure selected", lane)
		}
	}
}

func TestFailedResumeReservationRejectStaleAndRestart(t *testing.T) {
	t.Parallel()
	s, nc := workflowFixture(t)
	w := &Workflow{s: s, ctx: t.Context(), resumeConn: nc}
	c, err := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	w.client = c
	card := workflowCard(t, s)
	if err = w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(card.ID)
	out := client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed, Error: "connection reset"}
	if err = w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	a, _ = w.CardResult(card.ID)
	target := client.Target{TaskID: a.TaskID, AgentID: a.AgentID, RunID: a.RunID, AttemptID: a.RemoteAttemptID}
	sub, err := nc.Subscribe("bonnie.queries.agent", func(m *nats.Msg) {
		raw, _ := json.Marshal(client.Status{Version: 1, Target: target, State: runtime.RunFailed, TurnID: "turn", Seq: 20})
		_ = m.Respond(raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = nc.Flush()
	if err = w.resumeFailed(card.ID, false, "stale"); err == nil {
		t.Fatal("stale rendered action accepted")
	}
	if err = w.resumeFailed(card.ID, false, resumeToken(a)); err != nil {
		t.Fatal(err)
	}
	for name, action := range map[string]func(string) error{"resume": w.ResumeFailed, "retry": w.Retry, "reset": w.ResetCard, "delete": w.DeleteCard} {
		if err = action(card.ID); err == nil {
			t.Fatal("pending action allowed", name)
		}
	}
	// Old retained reports/events/resync may arrive while command delivery is
	// unknown. None may release the reservation or restore the failure.
	if err = w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	if err = w.event(t.Context(), client.StatusEvent{Version: 1, Target: target, EventID: "old", Seq: 21, State: runtime.RunFailed}); err != nil {
		t.Fatal(err)
	}
	pending, _ := w.CardResult(card.ID)
	if err = w.applyStatus(pending, client.Status{Version: 1, Target: target, State: runtime.RunFailed, Seq: 21}); err != nil {
		t.Fatal(err)
	}
	pending, _ = w.CardResult(card.ID)
	if pending.State != "running" || pending.OutcomeJSON != "" {
		t.Fatal("old delivery undid resume", pending)
	}
	// Simulate workflow owner reconstruction and unavailable host: durable
	// identity stays pending, no blind task dispatch or fresh retry.
	w = &Workflow{s: s, ctx: context.Background(), client: c, resumeConn: nc}
	w.dispatchFailedResumes(nc)
	var id, raw string
	_ = s.db.QueryRow(`SELECT id,request_json FROM failed_resumes WHERE task_id=?`, a.TaskID).Scan(&id, &raw)
	var req reportprotocol.FailedResume
	_ = json.Unmarshal([]byte(raw), &req)
	if err = w.Retry(card.ID); err == nil {
		t.Fatal("timeout enabled fresh work")
	}
	rejected := reportprotocol.FailedResumeReply{Version: 1, ID: id, Target: target, Status: "rejected", Error: "run changed"}
	for name, edit := range map[string]func(*reportprotocol.FailedResumeReply){
		"version":         func(r *reportprotocol.FailedResumeReply) { r.Version = 0 },
		"command":         func(r *reportprotocol.FailedResumeReply) { r.ID = "other" },
		"owner":           func(r *reportprotocol.FailedResumeReply) { r.Target.RunID = "other" },
		"status":          func(r *reportprotocol.FailedResumeReply) { r.Status = "unknown" },
		"missing outcome": func(r *reportprotocol.FailedResumeReply) { r.Status, r.Seq = "completed", req.Seq+1 },
		"stale cursor":    func(r *reportprotocol.FailedResumeReply) { r.Status, r.Seq, r.Outcome = "completed", req.Seq, &out },
		"outcome owner": func(r *reportprotocol.FailedResumeReply) {
			wrong := out
			wrong.RunID = "other"
			r.Status, r.Seq, r.Outcome = "completed", req.Seq+1, &wrong
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := rejected
			edit(&invalid)
			if e := w.finishFailedResume(id, invalid); e == nil {
				t.Fatal("invalid receipt released reservation")
			}
			var status string
			if e := s.db.QueryRow(`SELECT status FROM failed_resumes WHERE id=?`, id).Scan(&status); e != nil || status != "pending" {
				t.Fatal(status, e)
			}
		})
	}
	if err = w.finishFailedResume(id, rejected); err != nil {
		t.Fatal(err)
	}
	restored, _ := w.CardResult(card.ID)
	if restored.TaskJSON != a.TaskJSON || restored.OutcomeJSON != a.OutcomeJSON || !strings.Contains(restored.Error, "Resume rejected") {
		t.Fatal("rejection lost original failure", restored)
	}
	if err = w.Retry(card.ID); err != nil {
		t.Fatal("fresh fallback unavailable", err)
	}
	next, _ := w.CardResult(card.ID)
	if next.Number != 2 || next.TaskID == a.TaskID {
		t.Fatal("Retry no longer fresh", next)
	}
	// Late command completion cannot mutate the superseding execution.
	if err = w.finishFailedResume(id, reportprotocol.FailedResumeReply{Status: "completed", Outcome: &out}); err != nil {
		t.Fatal(err)
	}
}
