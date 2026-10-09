package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/presence"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func workflowFixture(t *testing.T) (*Store, *nats.Conn) {
	t.Helper()
	ns, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS not ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		nc.Close()
		ns.Shutdown()
		ns.WaitForShutdown()
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, nc
}
func waitWorkflow(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("workflow condition timed out")
}
func workflowCard(t *testing.T, s *Store) Card {
	t.Helper()
	p, e := s.CreateProjectDetails("test", "github", "https://github.com/example/repo", "main")
	if e != nil {
		t.Fatal(e)
	}
	c, e := s.CreateCard(p.ID, "build thing", "do it")
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func publishOutcome(t *testing.T, nc *nats.Conn, out client.Outcome) {
	t.Helper()
	js, e := nc.JetStream()
	if e != nil {
		t.Fatal(e)
	}
	data, e := json.Marshal(out)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = js.Publish("bonnie.results", data); e != nil {
		t.Fatal(e)
	}
}

func TestWorkflowDurableProtocolAndVerification(t *testing.T) {
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	w, e := NewWorkflow(t.Context(), s, nc)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	if e = w.MoveCard(c.ID, "Building", 0); e != nil {
		t.Fatal(e)
	}
	if e = w.MoveCard(c.ID, "Building", 9); e != nil {
		t.Fatal(e)
	}
	attempts, e := w.Attempts()
	if e != nil || len(attempts) != 1 {
		t.Fatalf("attempts %v %v", attempts, e)
	}
	a := attempts[0]
	if _, e = uuid.Parse(a.TaskID); e != nil {
		t.Fatal(e)
	}
	js, e := nc.JetStream()
	if e != nil {
		t.Fatal(e)
	}
	sub, e := js.PullSubscribe("bonnie.tasks", "fixture-worker")
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Error(err)
		}
	}()
	msgs, e := sub.Fetch(1, nats.MaxWait(5*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	var task client.Task
	if e = json.Unmarshal(msgs[0].Data, &task); e != nil {
		t.Fatal(e)
	}
	if e = msgs[0].AckSync(); e != nil {
		t.Fatal(e)
	}
	if task.TaskID != a.TaskID || task.Version != 1 || task.Text == "" {
		t.Fatalf("bad task %+v", task)
	}
	target := client.Target{TaskID: a.TaskID, WorkerID: "worker", RunID: "run", AttemptID: "remote"}
	ev := client.StatusEvent{Version: 1, Target: target, EventID: "accepted", Type: "task_accepted", Seq: 0, State: runtime.RunRunning}
	data, e := json.Marshal(ev)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = js.Publish("bonnie.events", data); e != nil {
		t.Fatal(e)
	}
	waitWorkflow(t, func() bool {
		a, e := w.CardResult(c.ID)
		if e != nil {
			t.Fatal(e)
		}
		return a.WorkerID == "worker"
	})
	out := client.Outcome{Version: 1, TaskID: a.TaskID, WorkerID: "worker", RunID: "run", AttemptID: "remote", State: runtime.RunCompleted, Response: `{"pr_url":"https://github.com/example/repo/pull/1"}`}
	publishOutcome(t, nc, out)
	waitWorkflow(t, func() bool {
		a, e := w.CardResult(c.ID)
		if e != nil {
			t.Fatal(e)
		}
		return a.PRURL != ""
	})
	ready, e := w.Ready(c.ID)
	if e != nil || ready {
		t.Fatal("unverified result became ready")
	}
	if e = w.MoveCard(c.ID, "Done", 0); e == nil {
		t.Fatal("manual Done permitted")
	}
	if e = w.MoveCard(c.ID, "Todo", 0); e == nil {
		t.Fatal("active cancellation permitted")
	}
	cards, e := s.Cards("")
	if e != nil {
		t.Fatal(e)
	}
	if cards[0].Status != "Building" {
		t.Fatal("completed without verification")
	}
	// A competing worker, duplicates and delayed statuses must not replace the report.
	bad := out
	bad.WorkerID = "other"
	bad.Response = `{"pr_url":"https://evil.example/pull/2"}`
	publishOutcome(t, nc, bad)
	publishOutcome(t, nc, out)
	ev.Seq = 50
	ev.EventID = "late"
	data, e = json.Marshal(ev)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = js.Publish("bonnie.events", data); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	restarted, e := NewWorkflow(t.Context(), s, nc)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	}()
	restarted.SetVerifier(func(ctx context.Context, a Attempt) (bool, error) {
		return a.PRURL == "https://github.com/example/repo/pull/1", nil
	})
	waitWorkflow(t, func() bool {
		ready, e := restarted.Ready(c.ID)
		if e != nil {
			t.Fatal(e)
		}
		return ready
	})
	cards, e = s.Cards("")
	if e != nil {
		t.Fatal(e)
	}
	if cards[0].Status != "Done" {
		t.Fatal("verified card not Done")
	}
	attempts, e = restarted.Attempts()
	if e != nil {
		t.Fatal(e)
	}
	if len(attempts) != 1 || attempts[0].State != "ready" {
		t.Fatalf("lost idempotency %+v", attempts)
	}
}

func TestWorkflowOutboxRestartAndOfflinePresence(t *testing.T) {
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	w, e := NewWorkflow(t.Context(), s, nc)
	if e != nil {
		t.Fatal(e)
	}
	// Stop loops before inserting the queued transaction using a separate live context.
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	w.ctx = context.Background()
	if e = w.MoveCard(c.ID, "Building", 0); e != nil {
		t.Fatal(e)
	}
	a, e := w.CardResult(c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if a.Published || a.State != "queued" {
		t.Fatal("outbox not queued")
	}
	restarted, e := NewWorkflow(t.Context(), s, nc)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	}()
	waitWorkflow(t, func() bool {
		a, e := restarted.CardResult(c.ID)
		if e != nil {
			t.Fatal(e)
		}
		return a.Published
	})
	b, e := restarted.CardResult(c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if b.TaskID != a.TaskID || b.TaskJSON != a.TaskJSON {
		t.Fatal("retry changed task")
	}
	workers, e := restarted.Workers(t.Context())
	if e != nil || len(workers) != 0 {
		t.Fatalf("offline presence %v %v", workers, e)
	}
	rec := presence.Record{Identity: presence.Identity{Worker: "fixture", Instance: "one"}, State: presence.Ready}
	if e = restarted.presence.Register(t.Context(), rec); e != nil {
		t.Fatal(e)
	}
	workers, e = restarted.Workers(t.Context())
	if e != nil || len(workers) != 1 {
		t.Fatalf("live presence %v %v", workers, e)
	}
	if e = restarted.presence.Unregister(t.Context(), rec.Identity); e != nil {
		t.Fatal(e)
	}
	workers, e = restarted.Workers(t.Context())
	if e != nil || len(workers) != 0 {
		t.Fatalf("offline presence %v %v", workers, e)
	}
	// Re-submit an ambiguous publish with exactly the same identity and payload.
	var task client.Task
	if e = json.Unmarshal([]byte(a.TaskJSON), &task); e != nil {
		t.Fatal(e)
	}
	receipt, e := restarted.client.Submit(t.Context(), task)
	if e != nil || !receipt.Duplicate {
		t.Fatalf("not deduplicated %+v %v", receipt, e)
	}
}

func TestWorkflowAtomicRollbackAndBlockedResults(t *testing.T) {
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	w, e := NewWorkflow(t.Context(), s, nc)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, e = s.db.Exec(`CREATE TRIGGER reject_attempt BEFORE INSERT ON workflow_attempts BEGIN SELECT RAISE(ABORT,'fixture'); END;`)
	if e != nil {
		t.Fatal(e)
	}
	if e = w.MoveCard(c.ID, "Building", 0); e == nil {
		t.Fatal("insertion failure ignored")
	}
	cards, e := s.Cards("")
	if e != nil {
		t.Fatal(e)
	}
	attempts, e := w.Attempts()
	if e != nil {
		t.Fatal(e)
	}
	if cards[0].Status != "Todo" || len(attempts) != 0 {
		t.Fatal("non-atomic move")
	}
	if _, e = s.db.Exec(`DROP TRIGGER reject_attempt`); e != nil {
		t.Fatal(e)
	}
	if e = w.MoveCard(c.ID, "Building", 0); e != nil {
		t.Fatal(e)
	}
	a, e := w.CardResult(c.ID)
	if e != nil {
		t.Fatal(e)
	}
	publishOutcome(t, nc, client.Outcome{Version: 1, TaskID: a.TaskID, WorkerID: "w", RunID: "r", AttemptID: "a", State: runtime.RunCompleted, Response: "I finished everything"})
	waitWorkflow(t, func() bool {
		a, e := w.CardResult(c.ID)
		if e != nil {
			t.Fatal(e)
		}
		return a.State == "blocked"
	})
	w.SetVerifier(func(context.Context, Attempt) (bool, error) {
		t.Error("invalid report reached verifier")
		return true, nil
	})
	time.Sleep(300 * time.Millisecond)
	ready, e := w.Ready(c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if ready {
		t.Fatal("malformed report became ready")
	}
}

func TestWorkflowRetryCreatesIsolatedAttempt(t *testing.T) {
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err = w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	first, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	publishOutcome(t, nc, client.Outcome{Version: 1, TaskID: first.TaskID, WorkerID: "w", RunID: "r", AttemptID: "remote", State: runtime.RunFailed, Error: "failed"})
	waitWorkflow(t, func() bool { a, e := w.CardResult(c.ID); return e == nil && a.OutcomeJSON != "" })
	if err = w.Retry(c.ID); err != nil {
		t.Fatal(err)
	}
	second, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Number != first.Number+1 || second.ID == first.ID || second.TaskID == first.TaskID || !strings.Contains(second.TaskJSON, taskBranch(c.Title, c.ID, second.Number)) {
		t.Fatalf("retry did not create isolated attempt: first=%+v second=%+v", first, second)
	}
	// A delayed successful result for the prior task must not complete the new attempt.
	publishOutcome(t, nc, client.Outcome{Version: 1, TaskID: first.TaskID, WorkerID: "w", RunID: "r", AttemptID: "remote", State: runtime.RunCompleted, Response: `{"pr_url":"https://github.com/example/repo/pull/1}`})
	time.Sleep(350 * time.Millisecond)
	latest, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != second.ID || latest.Ready || latest.OutcomeJSON != "" {
		t.Fatalf("late result affected latest attempt: %+v", latest)
	}
	if err = w.Retry(c.ID); err == nil {
		t.Fatal("retry of running attempt permitted")
	}
}

func TestWorkflowFailureAndWaitingRemainBuilding(t *testing.T) {
	for _, state := range []runtime.RunState{runtime.RunFailed, runtime.RunWaiting} {
		t.Run(string(state), func(t *testing.T) {
			s, nc := workflowFixture(t)
			c := workflowCard(t, s)
			w, e := NewWorkflow(t.Context(), s, nc)
			if e != nil {
				t.Fatal(e)
			}
			defer func() {
				if err := w.Close(); err != nil {
					t.Error(err)
				}
			}()
			if e = w.MoveCard(c.ID, "Building", 0); e != nil {
				t.Fatal(e)
			}
			a, e := w.CardResult(c.ID)
			if e != nil {
				t.Fatal(e)
			}
			out := client.Outcome{Version: 1, TaskID: a.TaskID, WorkerID: "w", RunID: "r", AttemptID: "a", State: state}
			if state == runtime.RunFailed {
				out.Error = "build failed"
			}
			publishOutcome(t, nc, out)
			waitWorkflow(t, func() bool {
				a, e := w.CardResult(c.ID)
				if e != nil {
					t.Fatal(e)
				}
				return a.OutcomeJSON != ""
			})
			a, e = w.CardResult(c.ID)
			if e != nil {
				t.Fatal(e)
			}
			cards, e := s.Cards("")
			if e != nil {
				t.Fatal(e)
			}
			if a.Ready || cards[0].Status != "Building" {
				t.Fatal("unsafe completion")
			}
			if state == runtime.RunWaiting {
				if e = w.Retry(c.ID); e == nil {
					t.Fatal("retry of waiting/nonterminal outcome permitted")
				}
				out.State = runtime.RunCompleted
				out.Response = `{"pr_url":"https://github.com/example/repo/pull/1"}`
				publishOutcome(t, nc, out)
				waitWorkflow(t, func() bool {
					a, e := w.CardResult(c.ID)
					if e != nil {
						t.Fatal(e)
					}
					return a.PRURL != ""
				})
			}
		})
	}
}
