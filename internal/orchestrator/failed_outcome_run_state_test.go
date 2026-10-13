package orchestrator

import (
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
)

const interruptedAnswer = "admitted answer resume interrupted; cannot auto continue safely"

// Replays JAW-F4AD: an agent restart makes BONNIE refuse to replay an admitted
// answer and publish an error outcome whose snapshot still reads running, but
// the runner recovers the run and later calls completeTask. The orchestrator
// must keep following the run and accept that completion.
func TestTransientOutcomeKeepsFollowingTheRun(t *testing.T) {
	t.Parallel()
	s, _ := workflowFixture(t)
	w := &Workflow{s: s, ctx: t.Context(), observeReport: fakeReportObserver}
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
	out := client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", State: runtime.RunRunning, Error: interruptedAnswer}
	// Delivered live and again by result recovery: neither ends the attempt.
	for range 2 {
		if err := w.result(t.Context(), out); err != nil {
			t.Fatal(err)
		}
	}
	a, _ = w.CardResult(c.ID)
	if a.State != "running" || a.RunState != "running" || a.OutcomeJSON != "" || runLabel(a) == "Failed" {
		t.Fatalf("transient outcome ended the attempt: %+v", a)
	}
	// The recovered run keeps reporting status and then completes.
	ev.EventID, ev.Seq = "recovered", 11
	if err := w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	r, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "turn", ToolCallID: "complete"}, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 55})
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := w.acceptReport(t.Context(), r); err != nil || !ack.Accepted {
		t.Fatalf("completeTask rejected after transient outcome: %+v %v", ack, err)
	}
	a, _ = w.CardResult(c.ID)
	if a.State != "blocked" || a.RunState != "completed" || a.PRURL != "https://github.com/example/repo/pull/55" || a.Error != "awaiting verification" {
		t.Fatalf("completion not recorded: %+v", a)
	}
	// A late replay of the transient outcome changes nothing.
	if err := w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	if after, _ := w.CardResult(c.ID); after.State != a.State || after.OutcomeJSON != a.OutcomeJSON || after.Error != a.Error {
		t.Fatalf("late transient outcome changed the attempt: %+v", after)
	}
}

// If the run never recovers, it must still be releasable: resync observes an
// idle running snapshot as interrupted, and Force stop then frees the card.
func TestTransientOutcomeOnDeadRunIsReleasable(t *testing.T) {
	t.Parallel()
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(c.ID)
	ev := client.StatusEvent{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", EventID: "running", Type: "run_state", Seq: 1, State: runtime.RunRunning}
	if err := w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if err := w.result(t.Context(), client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", State: runtime.RunRunning, Error: interruptedAnswer}); err != nil {
		t.Fatal(err)
	}
	a, _ = w.CardResult(c.ID)
	target := client.Target{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt"}
	if err := w.applyStatus(a, client.Status{Version: 1, Target: target, TurnID: "turn", State: runtime.RunRunning, Active: false, Seq: 2}); err != nil {
		t.Fatal(err)
	}
	a, _ = w.CardResult(c.ID)
	if runLabel(a) != "Interrupted" {
		t.Fatalf("dead run not shown as interrupted: %+v (%s)", a, runLabel(a))
	}
	card := Card{ID: c.ID, Status: "Building"}
	if !canAction(card, []Attempt{a}, "force-stop") {
		t.Fatal("force stop unavailable for a dead run")
	}
	if err := w.ForceStop(c.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.Retry(c.ID); err != nil {
		t.Fatalf("retry after force stop: %v", err)
	}
}

func TestTransientOutcomeDoesNotFailPlanning(t *testing.T) {
	t.Parallel()
	_, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	d := planningDetails(t, w, c.ID)
	turn := d.Turns[len(d.Turns)-1]
	if err := w.result(t.Context(), client.Outcome{Version: 1, TaskID: turn.ID, AgentID: "agent", RunID: "run", AttemptID: "attempt", State: runtime.RunRunning, Error: interruptedAnswer}); err != nil {
		t.Fatal(err)
	}
	d = planningDetails(t, w, c.ID)
	if d.Session.State == "failed" || d.Turns[len(d.Turns)-1].State == "failed" {
		t.Fatalf("transient outcome failed planning: session=%s turn=%s", d.Session.State, d.Turns[len(d.Turns)-1].State)
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
	for state, want := range map[runtime.RunState]bool{runtime.RunPending: true, runtime.RunRunning: true, runtime.RunFailed: false, runtime.RunCompleted: false, runtime.RunWaiting: false, runtime.RunInterrupted: false, runtime.RunCancelled: false} {
		if got := transientOutcome(client.Outcome{State: state}); got != want {
			t.Errorf("transientOutcome(%s) = %v", state, got)
		}
	}
}
