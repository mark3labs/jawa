package orchestrator

import (
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
)

// BONNIE can refuse an interrupted answer resume with a final failure whose
// run snapshot still reads running. The attempt must not look active forever.
func TestFailedOutcomeWithRunningSnapshotIsNotActive(t *testing.T) {
	t.Parallel()
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
	out := client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", State: runtime.RunRunning, Error: "admitted answer resume interrupted; cannot auto continue safely"}
	if err := w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	// A late running status must not revive the finalized attempt.
	ev.EventID, ev.Seq = "late", 11
	if err := w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	a, err = w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	card := Card{ID: c.ID, Status: "Building"}
	attempts := []Attempt{a}
	if a.State != "failed" || a.RunState != "failed" || attemptActive(a) || runLabel(a) != "Failed" {
		t.Fatalf("failed outcome still active: %+v", a)
	}
	if !canAction(card, attempts, "retry") || !canAction(card, attempts, "reset") || !canAction(card, attempts, "delete") {
		t.Fatalf("recovery actions unavailable: %+v", a)
	}
	// BONNIE refused to continue this run; Resume must stay unavailable.
	if canAction(card, attempts, "resume") {
		t.Fatalf("refused continuation offered as resume: %+v", a)
	}
	if err := w.Retry(c.ID); err != nil {
		t.Fatalf("retry rejected: %v", err)
	}
}

func TestOutcomeRunState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state runtime.RunState
		err   string
		want  string
	}{
		{runtime.RunRunning, "boom", "failed"},
		{runtime.RunRunning, "", "running"},
		{runtime.RunFailed, "boom", "failed"},
		{runtime.RunCompleted, "", "completed"},
		{runtime.RunWaiting, "", "waiting"},
		{runtime.RunInterrupted, "boom", "interrupted"},
		{runtime.RunCancelled, "boom", "cancelled"},
	} {
		if got := outcomeRunState(client.Outcome{State: tc.state, Error: tc.err}); got != tc.want {
			t.Errorf("%s/%q = %s, want %s", tc.state, tc.err, got, tc.want)
		}
	}
}
