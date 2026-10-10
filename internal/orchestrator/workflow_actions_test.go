package orchestrator

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
)

// No dispatch loops: action tests exercise the durable transaction before publish.
func actionWorkflow(t *testing.T) *Workflow {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "actions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := migrateWorkflow(s); err != nil {
		t.Fatal(err)
	}
	return &Workflow{s: s, ctx: t.Context()}
}

func actionAttempt(t *testing.T, w *Workflow, c Card, state, runState string) Attempt {
	t.Helper()
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET state=?,run_state=?,outcome_json='{}' WHERE card_id=?`, state, runState, c.ID); err != nil {
		t.Fatal(err)
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func assertActionLane(t *testing.T, w *Workflow, project, lane string, ids ...string) {
	t.Helper()
	cards, err := w.s.Cards(project)
	if err != nil {
		t.Fatal(err)
	}
	var found []Card
	for _, c := range cards {
		if c.Status == lane {
			found = append(found, c)
		}
	}
	if len(found) != len(ids) {
		t.Fatalf("%s lane: %+v, want %v", lane, found, ids)
	}
	for i, c := range found {
		if c.ID != ids[i] || c.Position != i {
			t.Fatalf("%s lane: %+v, want %v", lane, found, ids)
		}
	}
}

func TestWorkflowLegacyActions(t *testing.T) {
	for _, action := range []string{"retry", "move", "reset", "delete"} {
		t.Run(action, func(t *testing.T) {
			w := actionWorkflow(t)
			c := workflowCard(t, w.s)
			if err := w.s.MoveCard(c.ID, "Building", 0); err != nil {
				t.Fatal(err)
			}
			var err error
			switch action {
			case "retry":
				err = w.Retry(c.ID)
			case "move":
				err = w.MoveCard(c.ID, "Todo", 0)
			case "reset":
				err = w.ResetCard(c.ID)
			case "delete":
				err = w.DeleteCard(c.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if action == "retry" {
				a, err := w.CardResult(c.ID)
				if err != nil || a.Number != 1 || a.State != "queued" {
					t.Fatalf("initial attempt: %+v, %v", a, err)
				}
				if err := w.Retry(c.ID); err == nil {
					t.Fatal("duplicate retry allowed")
				}
			} else {
				if _, err := w.CardResult(c.ID); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("CardResult changed: %v", err)
				}
				if action == "delete" {
					assertActionLane(t, w, c.ProjectID, "Todo")
				} else {
					assertActionLane(t, w, c.ProjectID, "Todo", c.ID)
				}
				assertActionLane(t, w, c.ProjectID, "Building")
			}
		})
	}
}

func TestWorkflowActionGuards(t *testing.T) {
	for _, tc := range []struct {
		state, run    string
		reset, delete bool
	}{
		{"queued", "", false, false},
		{"submitted", "", false, false},
		{"running", "running", false, false},
		{"waiting", "waiting", false, false},
		{"blocked", "waiting", false, false},
		{"failed", "failed", true, true},
		{"blocked", "completed", true, true},
		{"ready", "completed", false, true},
	} {
		for _, action := range []string{"reset", "delete"} {
			t.Run(tc.state+"/"+tc.run+"/"+action, func(t *testing.T) {
				w := actionWorkflow(t)
				c := workflowCard(t, w.s)
				a := actionAttempt(t, w, c, tc.state, tc.run)
				// Even an already published queued attempt must block deletion.
				if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET published=1 WHERE id=?`, a.ID); err != nil {
					t.Fatal(err)
				}
				want := tc.reset
				var err error
				if action == "reset" {
					err = w.ResetCard(c.ID)
				} else {
					want = tc.delete
					err = w.DeleteCard(c.ID)
				}
				if (err == nil) != want {
					t.Fatalf("allowed=%v: %v", want, err)
				}
				if !want {
					assertActionLane(t, w, c.ProjectID, "Building", c.ID)
				} else if action == "reset" {
					assertActionLane(t, w, c.ProjectID, "Todo", c.ID)
					latest, err := w.CardResult(c.ID)
					if err != nil || latest.ID != a.ID || latest.OutcomeJSON != a.OutcomeJSON {
						t.Fatalf("history changed: %+v %v", latest, err)
					}
					if err := w.MoveCard(c.ID, "Building", 0); err != nil {
						t.Fatal(err)
					}
					latest, err = w.CardResult(c.ID)
					if err != nil || latest.Number != 2 {
						t.Fatalf("new attempt: %+v %v", latest, err)
					}
				} else {
					if _, err := w.CardResult(c.ID); !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("attempt not cascaded: %v", err)
					}
					if err := w.result(t.Context(), client.Outcome{TaskID: a.TaskID, State: runtime.RunCompleted}); err != nil {
						t.Fatal(err)
					}
					if err := w.event(t.Context(), client.StatusEvent{TaskID: a.TaskID, State: runtime.RunRunning}); err != nil {
						t.Fatal(err)
					}
					attempts, err := w.Attempts()
					if err != nil || len(attempts) != 0 {
						t.Fatalf("late messages recreated history: %+v %v", attempts, err)
					}
				}
			})
		}
	}
}

func TestWorkflowActionsUnknownCard(t *testing.T) {
	w := actionWorkflow(t)
	for name, action := range map[string]func(string) error{
		"retry": w.Retry, "reset": w.ResetCard, "delete": w.DeleteCard,
		"move": func(id string) error { return w.MoveCard(id, "Building", 0) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := action("missing"); err == nil || err.Error() != "card not found" {
				t.Fatalf("missing card: %v", err)
			}
		})
	}
}

func TestWorkflowConcurrentRetry(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "legacy"}[legacy], func(t *testing.T) {
			w := actionWorkflow(t)
			c := workflowCard(t, w.s)
			want := 2
			if legacy {
				want = 1
				if err := w.s.MoveCard(c.ID, "Building", 0); err != nil {
					t.Fatal(err)
				}
			} else {
				actionAttempt(t, w, c, "failed", "failed")
			}
			// A second pool verifies the database lock, not just pool serialization.
			var path string
			if err := w.s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
				t.Fatal(err)
			}
			other, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := other.Close(); err != nil {
					t.Error(err)
				}
			})
			w2 := &Workflow{s: other, ctx: t.Context()}
			start := make(chan struct{})
			results := make(chan error, 12)
			var wg sync.WaitGroup
			for i := range 12 {
				agent := w
				if i%2 == 1 {
					agent = w2
				}
				wg.Go(func() { <-start; results <- agent.Retry(c.ID) })
			}
			close(start)
			wg.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				}
			}
			attempts, err := w.Attempts()
			if err != nil || len(attempts) != want || successes != 1 {
				t.Fatalf("successes=%d attempts=%+v err=%v", successes, attempts, err)
			}
		})
	}
}

func TestWorkflowDeleteCompactsLane(t *testing.T) {
	for _, lane := range []string{"Todo", "Building", "Done"} {
		t.Run(lane, func(t *testing.T) {
			w := actionWorkflow(t)
			first := workflowCard(t, w.s)
			middle, err := w.s.CreateCard(first.ProjectID, "middle", "")
			if err != nil {
				t.Fatal(err)
			}
			last, err := w.s.CreateCard(first.ProjectID, "last", "")
			if err != nil {
				t.Fatal(err)
			}
			for i, c := range []Card{first, middle, last} {
				if err := w.s.MoveCard(c.ID, lane, i); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.DeleteCard(middle.ID); err != nil {
				t.Fatal(err)
			}
			assertActionLane(t, w, first.ProjectID, lane, first.ID, last.ID)
			if err := w.DeleteCard(middle.ID); err == nil || err.Error() != "card not found" {
				t.Fatalf("duplicate delete: %v", err)
			}
		})
	}
}

func TestWorkflowResetCompactsLanesAndRollback(t *testing.T) {
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	a := actionAttempt(t, w, c, "blocked", "completed")
	other, err := w.s.CreateCard(c.ProjectID, "other", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.s.MoveCard(other.ID, "Building", 1); err != nil {
		t.Fatal(err)
	}
	todo, err := w.s.CreateCard(c.ProjectID, "todo", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.db.Exec(`CREATE TRIGGER reject_reset BEFORE UPDATE OF status ON cards WHEN NEW.status='Todo' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := w.ResetCard(c.ID); err == nil {
		t.Fatal("reset failure ignored")
	}
	assertActionLane(t, w, c.ProjectID, "Building", c.ID, other.ID)
	if _, err := w.s.db.Exec(`DROP TRIGGER reject_reset`); err != nil {
		t.Fatal(err)
	}
	if err := w.ResetCard(c.ID); err != nil {
		t.Fatal(err)
	}
	assertActionLane(t, w, c.ProjectID, "Todo", todo.ID, c.ID)
	assertActionLane(t, w, c.ProjectID, "Building", other.ID)
	latest, err := w.CardResult(c.ID)
	if err != nil || latest != a {
		t.Fatalf("history changed: %+v %v", latest, err)
	}
	if _, err := w.s.db.Exec(`CREATE TRIGGER reject_delete BEFORE DELETE ON cards BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := w.DeleteCard(c.ID); err == nil {
		t.Fatal("delete failure ignored")
	}
	assertActionLane(t, w, c.ProjectID, "Todo", todo.ID, c.ID)
	latest, err = w.CardResult(c.ID)
	if err != nil || latest != a {
		t.Fatalf("failed delete lost history: %+v %v", latest, err)
	}
}

func TestWorkflowActionsGuardAllAttempts(t *testing.T) {
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	first := actionAttempt(t, w, c, "failed", "failed")
	if err := w.Retry(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET state='failed',run_state='failed',outcome_json='{}' WHERE card_id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET state='queued',run_state='',outcome_json='' WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	for name, action := range map[string]func(string) error{"retry": w.Retry, "reset": w.ResetCard, "delete": w.DeleteCard} {
		if err := action(c.ID); err == nil {
			t.Fatalf("%s ignored active historical attempt", name)
		}
	}
	assertActionLane(t, w, c.ProjectID, "Building", c.ID)
}
