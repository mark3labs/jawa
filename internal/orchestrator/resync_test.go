package orchestrator

import (
	"encoding/json"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
)

func TestResyncOrphanedRun(t *testing.T) {
	s, nc := workflowFixture(t)
	if err := migrateWorkflow(s); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context(), client: cl}
	c := workflowCard(t, s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := client.Target{TaskID: a.TaskID, AgentID: "w", RunID: "r", AttemptID: "a"}
	ev := client.StatusEvent{Version: 1, Target: target, Type: "run_state", EventID: "running", Seq: 10, State: runtime.RunRunning}
	if err := w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	status := client.Status{Version: 1, Target: target, Seq: 10, State: runtime.RunRunning, Active: false}
	sub, err := nc.Subscribe("bonnie.queries.w", func(m *nats.Msg) {
		data, err := json.Marshal(status)
		if err != nil {
			return
		}
		_ = m.Respond(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	w.resyncRuns(nc)
	a, err = w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != "blocked" || a.RunState != "running" || a.Error == "" {
		t.Fatalf("orphan not surfaced: %+v", a)
	}
	if err := sub.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	// The owner can become active without a new journal state sequence.
	status.Active = true
	if err := w.applyStatus(a, status); err != nil {
		t.Fatal(err)
	}
	a, err = w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != "running" || a.Error != "" {
		t.Fatalf("recovery not surfaced: %+v", a)
	}
	status.Seq = 9
	status.State = runtime.RunCancelled
	if err := w.applyStatus(a, status); err != nil {
		t.Fatal(err)
	}
	status.Seq = 11
	status.RunID = "other"
	if err := w.applyStatus(a, status); err != nil {
		t.Fatal(err)
	}
	latest, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != "running" || latest.EventSeq != 10 {
		t.Fatalf("unsafe snapshot accepted: %+v", latest)
	}
	w.resyncRuns(nc) // Unreachable owner does not imply cancellation or failure.
	latest, err = w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != "running" {
		t.Fatalf("offline owner changed state: %+v", latest)
	}
}
