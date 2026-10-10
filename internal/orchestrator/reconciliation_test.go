package orchestrator

import (
	"context"
	"encoding/json"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
)

func conflictCount(t *testing.T, s *Store, task string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM workflow_conflicts WHERE task_id=?`, task).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReconcileRetainedNATSOutcome(t *testing.T) {
	s, nc := workflowFixture(t)
	card := workflowCard(t, s)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err = w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, err := w.CardResult(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev := client.StatusEvent{Version: 1, TaskID: a.TaskID, WorkerID: "worker-A", RunID: "run-A", AttemptID: "attempt-A", EventID: "accepted-A", Type: "task_accepted", Seq: 12, State: runtime.RunRunning}
	if err = w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	b := client.Outcome{Version: 1, TaskID: a.TaskID, WorkerID: "worker-B", RunID: "run-B", AttemptID: "attempt-B", State: runtime.RunCompleted, Response: `{"pr_number":2}`}
	publishOutcome(t, nc, b)
	waitWorkflow(t, func() bool { return conflictCount(t, s, a.TaskID) == 1 })
	pinned, err := w.CardResult(card.ID)
	if err != nil || pinned.WorkerID != "worker-A" || pinned.OutcomeJSON != "" {
		t.Fatalf("pin changed: %+v %v", pinned, err)
	}
	var raw, reason, worker, run, remote string
	var received int64
	if err = s.db.QueryRow(`SELECT outcome_json,reason,worker_id,run_id,remote_attempt_id,received_at FROM workflow_conflicts WHERE task_id=?`, a.TaskID).Scan(&raw, &reason, &worker, &run, &remote, &received); err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(b)
	if raw != string(expected) || reason == "" || received <= 0 || worker != b.WorkerID || run != b.RunID || remote != b.AttemptID {
		t.Fatal("incomplete conflict retention")
	}
	// Repeated identical deliveries preserve the first evidence receipt.
	if err = w.result(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if conflictCount(t, s, a.TaskID) != 1 {
		t.Fatal("duplicate delivery inserted evidence")
	}
	ev.Target = client.Target{TaskID: a.TaskID, WorkerID: b.WorkerID, RunID: b.RunID, AttemptID: b.AttemptID}
	ev.EventID = "accepted-B"
	for range 2 {
		if err = w.event(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
	}
	var events int
	if err = s.db.QueryRow(`SELECT count(*) FROM workflow_execution_events WHERE task_id=?`, a.TaskID).Scan(&events); err != nil || events != 2 {
		t.Fatalf("events: %d %v", events, err)
	}
	if err = w.Reconcile(a.TaskID, b.RunID, b.AttemptID); err != nil {
		t.Fatal(err)
	}
	selected, err := w.CardResult(card.ID)
	if err != nil || selected.WorkerID != b.WorkerID || selected.PRURL == "" || selected.State != "blocked" || selected.Ready || selected.RunState != "completed" || selected.EventSeq != -1 {
		t.Fatalf("selected: %+v %v", selected, err)
	}
	var previous, chosen string
	if err = s.db.QueryRow(`SELECT previous_worker_id,selected_worker_id FROM workflow_reconciliations WHERE task_id=?`, a.TaskID).Scan(&previous, &chosen); err != nil || previous != "worker-A" || chosen != "worker-B" {
		t.Fatalf("audit: %s %s %v", previous, chosen, err)
	}
	old := b
	old.WorkerID, old.RunID, old.AttemptID = "worker-A", "run-A", "attempt-A"
	old.State, old.Error = runtime.RunFailed, "old failure"
	publishOutcome(t, nc, old)
	waitWorkflow(t, func() bool { return conflictCount(t, s, a.TaskID) == 2 })
	current, _ := w.CardResult(card.ID)
	if current.OutcomeJSON != selected.OutcomeJSON {
		t.Fatal("late result replaced selected result")
	}
	if err = w.markReady(selected); err != nil {
		t.Fatal(err)
	}
	if err = w.Reconcile(a.TaskID, old.RunID, old.AttemptID); err == nil {
		t.Fatal("ready reconciliation permitted")
	}
	// Even after readiness, mismatches remain evidence.
	if err = w.result(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if conflictCount(t, s, a.TaskID) != 2 {
		t.Fatal("ready conflict lost")
	}
}

func stoppedReconciliationWorkflow(t *testing.T) (*Workflow, Card) {
	t.Helper()
	s, nc := workflowFixture(t)
	card := workflowCard(t, s)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	w.ctx = t.Context()
	if err = w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	return w, card
}

func TestReconcileGuardsAndFailure(t *testing.T) {
	for _, kind := range []string{"unknown", "empty", "waiting", "cancelled", "missing-worker", "superseded", "todo", "ambiguous", "failed"} {
		t.Run(kind, func(t *testing.T) {
			w, card := stoppedReconciliationWorkflow(t)
			a, _ := w.CardResult(card.ID)
			ev := client.StatusEvent{TaskID: a.TaskID, WorkerID: "A", RunID: "A", AttemptID: "A", EventID: "A", Seq: 1, State: runtime.RunRunning}
			if err := w.event(t.Context(), ev); err != nil {
				t.Fatal(err)
			}
			b := client.Outcome{TaskID: a.TaskID, WorkerID: "B", RunID: "B", AttemptID: "B", State: runtime.RunFailed, Error: "failure"}
			switch kind {
			case "waiting":
				b.State = runtime.RunWaiting
			case "cancelled":
				b.State = runtime.RunCancelled
			case "missing-worker":
				b.WorkerID = ""
			}
			if err := w.result(t.Context(), b); err != nil {
				t.Fatal(err)
			}
			if kind == "superseded" || kind == "todo" {
				old := b
				old.WorkerID, old.RunID, old.AttemptID = "A", "A", "A"
				if err := w.result(t.Context(), old); err != nil {
					t.Fatal(err)
				}
				if kind == "superseded" {
					if err := w.Retry(card.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := w.ResetCard(card.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if kind == "ambiguous" {
				b.WorkerID = "other-worker"
				if err := w.result(t.Context(), b); err != nil {
					t.Fatal(err)
				}
			}
			run := "B"
			if kind == "unknown" {
				run = "unknown"
			}
			if kind == "empty" {
				run = ""
			}
			err := w.Reconcile(a.TaskID, run, "B")
			if kind == "failed" {
				if err != nil {
					t.Fatal(err)
				}
				got, _ := w.CardResult(card.ID)
				if got.State != "failed" || got.Ready || got.Error != "failure" {
					t.Fatalf("%+v", got)
				}
			} else {
				if err == nil {
					t.Fatal("guard permitted reconciliation")
				}
				got, scanErr := scanAttempt(w.s.db.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE task_id=?`, a.TaskID))
				if scanErr != nil || got.WorkerID != "A" {
					t.Fatalf("guard changed identity: %+v %v", got, scanErr)
				}
				var count int
				if err = w.s.db.QueryRow(`SELECT count(*) FROM workflow_reconciliations`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("guard wrote audit: %d %v", count, err)
				}
			}
		})
	}
}

func TestRecoveryPreviouslyAckedResult(t *testing.T) {
	s, nc := workflowFixture(t)
	card := workflowCard(t, s)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(card.ID)
	ev := client.StatusEvent{TaskID: a.TaskID, WorkerID: "A", RunID: "A", AttemptID: "A", EventID: "A", Seq: 1, State: runtime.RunRunning}
	if err = w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	b := client.Outcome{Version: 1, TaskID: a.TaskID, WorkerID: "B", RunID: "B", AttemptID: "B", State: runtime.RunCompleted, Response: `{"pr_number":2}`}
	publishOutcome(t, nc, b)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	stream := client.DefaultResultStreamName("bonnie.results")
	waitWorkflow(t, func() bool {
		info, e := js.ConsumerInfo(stream, "jawa-workflow-results")
		return e == nil && info.AckFloor.Stream >= 1
	})
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-migration handler which acknowledged but discarded B.
	if _, err = s.db.Exec(`DELETE FROM workflow_conflicts`); err != nil {
		t.Fatal(err)
	}
	before, err := js.ConsumerInfo(stream, "jawa-workflow-results")
	if err != nil {
		t.Fatal(err)
	}
	w, err = NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if conflictCount(t, s, a.TaskID) != 1 {
		t.Fatal("startup did not recover acked result")
	}
	var receipt int64
	if err = s.db.QueryRow("SELECT received_at FROM workflow_conflicts WHERE task_id=?", a.TaskID).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err = w.RecoverResults(t.Context(), nc); err != nil {
			t.Fatal(err)
		}
	}
	if conflictCount(t, s, a.TaskID) != 1 {
		t.Fatal("recovery duplicated evidence")
	}
	var afterReceipt int64
	if err = s.db.QueryRow("SELECT received_at FROM workflow_conflicts WHERE task_id=?", a.TaskID).Scan(&afterReceipt); err != nil {
		t.Fatal(err)
	}
	if receipt != afterReceipt {
		t.Fatal("recovery changed first receipt")
	}

	after, err := js.ConsumerInfo(stream, "jawa-workflow-results")
	if err != nil || before.AckFloor.Stream != after.AckFloor.Stream || before.AckFloor.Consumer != after.AckFloor.Consumer {
		t.Fatalf("consumer reset: %v", err)
	}
	if err = w.Reconcile(a.TaskID, "B", "B"); err != nil {
		t.Fatal(err)
	}
	got, _ := w.CardResult(card.ID)
	if got.State != "blocked" || got.Ready || got.WorkerID != "B" {
		t.Fatalf("recovered result: %+v", got)
	}
	info, err := js.StreamInfo(stream)
	if err != nil || info.State.Msgs != 1 {
		t.Fatal("recovery removed retained messages")
	}
	// Manual recovery must honor caller cancellation without changing anything.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = w.RecoverResults(ctx, nc); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestReconcileAtomicRollbackAndStaleVerification(t *testing.T) {
	w, card := stoppedReconciliationWorkflow(t)
	a, _ := w.CardResult(card.ID)
	old := client.Outcome{TaskID: a.TaskID, WorkerID: "A", RunID: "A", AttemptID: "A", State: runtime.RunCompleted, Response: `{"pr_number":1}`}
	if err := w.result(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	pinned, _ := w.CardResult(card.ID)
	b := old
	b.WorkerID, b.RunID, b.AttemptID = "B", "B", "B"
	b.State = runtime.RunWaiting
	if err := w.result(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	b.State = runtime.RunCompleted
	b.Response = `{"pr_number":2}`
	if err := w.result(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	// An audit failure must roll back the identity switch and result application.
	if _, err := w.s.db.Exec(`CREATE TRIGGER reject_reconciliation BEFORE INSERT ON workflow_reconciliations BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := w.Reconcile(a.TaskID, "B", "B"); err == nil {
		t.Fatal("audit failure ignored")
	}
	current, _ := w.CardResult(card.ID)
	if current.OutcomeJSON != pinned.OutcomeJSON || current.WorkerID != pinned.WorkerID {
		t.Fatal("partial reconciliation committed")
	}
	if _, err := w.s.db.Exec(`DROP TRIGGER reject_reconciliation`); err != nil {
		t.Fatal(err)
	}
	if err := w.Reconcile(a.TaskID, "B", "B"); err != nil {
		t.Fatal(err)
	}
	// A verifier that began on A before reconciliation cannot approve B.
	if err := w.markReady(pinned); err != nil {
		t.Fatal(err)
	}
	current, _ = w.CardResult(card.ID)
	if current.Ready || current.WorkerID != "B" || current.PRURL != "https://github.com/example/repo/pull/2" {
		t.Fatalf("stale verification applied: %+v", current)
	}
}
