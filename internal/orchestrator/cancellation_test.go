package orchestrator

import (
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
)

func TestWorkflowCancelledRunRestarts(t *testing.T) {
	for _, action := range []string{"resume", "retry", "reset"} {
		t.Run(action, func(t *testing.T) {
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
			ev := client.StatusEvent{Version: 1, TaskID: a.TaskID, AgentID: "w", RunID: "r", AttemptID: "a", Type: "run_state", EventID: "cancel", Seq: 10, State: runtime.RunCancelled}
			if err := w.event(t.Context(), ev); err != nil {
				t.Fatal(err)
			}
			a, err = w.CardResult(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if a.State != "failed" || attemptActive(a) || runLabel(a) != "Cancelled" || !terminalFailure(a) {
				t.Fatalf("cancelled status: %+v", a)
			}
			out := client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "w", RunID: "r", AttemptID: "a", State: runtime.RunCancelled}
			if err := w.result(t.Context(), out); err != nil {
				t.Fatal(err)
			}
			if action == "retry" {
				if err := w.Retry(c.ID); err != nil {
					t.Fatal(err)
				}
			}
			if action == "reset" {
				if err := w.ResetCard(c.ID); err != nil {
					t.Fatal(err)
				}
			}
			// A different execution is not an implicit restart of the pinned run.
			ev.EventID, ev.Seq, ev.State, ev.RunID = "other", 100, runtime.RunRunning, "other"
			if err := w.event(t.Context(), ev); err != nil {
				t.Fatal(err)
			}
			out.RunID = "other"
			if err := w.result(t.Context(), out); err != nil {
				t.Fatal(err)
			}
			out.RunID = "r"
			ev.EventID, ev.Seq, ev.State, ev.RunID = "resume", 11, runtime.RunRunning, "r"
			if err := w.event(t.Context(), ev); err != nil {
				t.Fatal(err)
			}
			// Replayed cancellation must not undo a newer running event.
			if err := w.result(t.Context(), out); err != nil {
				t.Fatal(err)
			}
			ev.EventID, ev.Seq, ev.State = "cancel", 10, runtime.RunCancelled
			if err := w.event(t.Context(), ev); err != nil {
				t.Fatal(err)
			}
			a, err = w.CardResult(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if action == "resume" && (a.State != "running" || a.RunState != "running" || a.Error != "" || !attemptActive(a)) {
				t.Fatalf("resume ignored: %+v", a)
			}
			out.State, out.Response = runtime.RunCompleted, `{"pr_number":123}`
			if err := w.result(t.Context(), out); err != nil {
				t.Fatal(err)
			}
			a, err = w.CardResult(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "resume":
				if a.State != "blocked" || a.RunState != "completed" || a.PRURL == "" {
					t.Fatalf("completion ignored: %+v", a)
				}
			case "retry":
				if a.Number != 2 || a.OutcomeJSON != "" {
					t.Fatalf("old run affected retry: %+v", a)
				}
			case "reset":
				if a.RunState != "cancelled" {
					t.Fatalf("reset run revived: %+v", a)
				}
			}
		})
	}
}
