package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func persistConflictAttempt(t *testing.T, s *Store, a Attempt) {
	t.Helper()
	_, err := s.db.Exec(`UPDATE cards SET status='Done' WHERE id=?`, a.CardID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,state,ready,pr_url,created_at,updated_at) VALUES(?,?,?,?,?,'ready',1,?,0,0)`, a.ID, a.CardID, a.Number, a.TaskID, a.TaskJSON, a.PRURL)
	if err != nil {
		t.Fatal(err)
	}
}

func TestConflictMonitorPersistenceAndFreshness(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	w := &Workflow{s: s}
	calls := 0
	state := "conflict"
	fail := false
	w.SetProviderChecks(nil, func(ctx context.Context, got Attempt) (PRCheck, error) {
		calls++
		if got.ID != a.ID {
			t.Fatal("wrong attempt")
		}
		if fail {
			return PRCheck{State: "conflict"}, errors.New("secret transport detail")
		}
		return PRCheck{State: state, HeadSHA: "head"}, nil
	})
	if _, err := w.CardPRCheck(a.CardID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing check: %v", err)
	}
	for _, want := range []string{"conflict", "clear", "unknown", "error"} {
		state, fail = want, want == "error"
		if _, err := s.db.Exec(`UPDATE workflow_pr_checks SET checked_at=0`); err != nil {
			t.Fatal(err)
		}
		if err := w.PollPRConflicts(context.Background()); err != nil {
			t.Fatal(err)
		}
		p, err := w.CardPRCheck(a.CardID)
		if err != nil || p.State != want || p.AttemptID != a.ID || p.PRURL != a.PRURL || p.CheckedAt.IsZero() {
			t.Fatalf("check=%+v err=%v", p, err)
		}
		if want == "error" && p.Error != "provider check failed" {
			t.Fatal("raw error persisted")
		}
		before := calls
		if err := w.PollPRConflicts(context.Background()); err != nil || calls != before {
			t.Fatalf("freshness calls=%d err=%v", calls, err)
		}
	}
	latest, err := w.CardResult(a.CardID)
	if err != nil || !latest.Ready || latest.State != "ready" {
		t.Fatalf("readiness mutated: %+v %v", latest, err)
	}
	// New workflow reads persisted state without any provider calls.
	restarted := &Workflow{s: s}
	if p, err := restarted.CardPRCheck(a.CardID); err != nil || p.State != "error" {
		t.Fatalf("persistent check: %+v %v", p, err)
	}
	if _, err := s.db.Exec(`UPDATE cards SET status='Fix' WHERE id=?`, a.CardID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CardPRCheck(a.CardID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("repair exposed stale check: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM cards WHERE id=?`, a.CardID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM workflow_pr_checks`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascade: %d %v", count, err)
	}
}

func TestConflictMonitorDiscardsRaces(t *testing.T) {
	for _, change := range []string{"lane", "attempt", "project"} {
		t.Run(change, func(t *testing.T) {
			s, a := conflictFixture(t, "github")
			persistConflictAttempt(t, s, a)
			w := &Workflow{s: s}
			w.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) {
				var err error
				switch change {
				case "lane":
					_, err = s.db.Exec(`UPDATE cards SET status='Fix' WHERE id=?`, a.CardID)
				case "attempt":
					_, err = s.db.Exec(`INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,created_at,updated_at) VALUES('next',?,2,'next','{}',0,0)`, a.CardID)
				case "project":
					_, err = s.db.Exec(`UPDATE projects SET base_branch='new'`)
				}
				if err != nil {
					t.Fatal(err)
				}
				return PRCheck{State: "conflict"}, nil
			})
			if err := w.PollPRConflicts(context.Background()); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM workflow_pr_checks`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale write: %d %v", count, err)
			}
		})
	}
}

func TestConflictMonitorBatchAndCancellation(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	var project string
	if err := s.db.QueryRow(`SELECT project_id FROM cards WHERE id=?`, a.CardID).Scan(&project); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		c, err := s.CreateCard(project, fmt.Sprintf("card %d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		extra := a
		extra.ID, extra.TaskID, extra.CardID = fmt.Sprintf("a%d", i), fmt.Sprintf("a%d", i), c.ID
		persistConflictAttempt(t, s, extra)
	}
	w := &Workflow{s: s}
	calls := 0
	w.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) { calls++; return PRCheck{State: "unknown"}, nil })
	if err := w.PollPRConflicts(context.Background()); err != nil || calls != 16 {
		t.Fatalf("batch calls=%d err=%v", calls, err)
	}
	if err := w.PollPRConflicts(context.Background()); err != nil || calls != 21 {
		t.Fatalf("starved calls=%d err=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.PollPRConflicts(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	// Provider must receive a bounded deadline and can unblock on cancellation.
	if _, err := s.db.Exec(`UPDATE workflow_pr_checks SET checked_at=0`); err != nil {
		t.Fatal(err)
	}
	w.SetProviderChecks(nil, func(ctx context.Context, _ Attempt) (PRCheck, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("missing bounded deadline")
		}
		cancel()
		<-ctx.Done()
		return PRCheck{}, ctx.Err()
	})
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if err := w.PollPRConflicts(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("inflight cancel: %v", err)
	}
}
