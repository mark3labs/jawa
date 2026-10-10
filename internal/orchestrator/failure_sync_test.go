package orchestrator

import (
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"testing"
)

func TestFailureReportMissingAgentIsVisibleWithoutRepinning(t *testing.T) {
	s, _ := workflowFixture(t)
	if err := migrateWorkflow(s); err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context()}
	c := workflowCard(t, s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev := client.StatusEvent{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", EventID: "running", Type: "run_state", Seq: 10, State: runtime.RunRunning}
	if err := w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	out := client.Outcome{Version: 1, TaskID: a.TaskID, RunID: "run", AttemptID: "attempt", State: runtime.RunFailed, Error: "provider rejected empty message"}
	if err := w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	a, err = w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runLabel(a) != "Needs reconciliation" || runTone(a) != "blocked" || a.AgentID != "agent" || a.RunState != "running" || a.OutcomeJSON != "" || a.Ready {
		t.Fatalf("conflict hidden or accepted: %+v", a)
	}
	if err := w.Retry(c.ID); err == nil {
		t.Fatal("ambiguous run retry allowed")
	}
	out.AgentID = "agent"
	if err := w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	a, err = w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != "failed" || a.RunState != "failed" || runLabel(a) != "Failed" {
		t.Fatalf("valid failure not applied: %+v", a)
	}
}
