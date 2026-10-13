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
	for _, change := range []string{"lane", "attempt", "project", "archive"} {
		t.Run(change, func(t *testing.T) {
			s, a := conflictFixture(t, "github")
			persistConflictAttempt(t, s, a)
			w := &Workflow{s: s}
			w.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) {
				var err error
				switch change {
				case "archive":
					err = s.SetCardArchived(a.CardID, true)
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

// addArchivedConflictAttempt includes optional historical evidence, including queued work.
func addArchivedConflictAttempt(t *testing.T, s *Store, a Attempt, project, id, state string) Attempt {
	t.Helper()
	c, err := s.CreateCard(project, id, "")
	if err != nil {
		t.Fatal(err)
	}
	a.ID, a.TaskID, a.CardID = id, id, c.ID
	persistConflictAttempt(t, s, a)
	if state != "" {
		_, err = s.db.Exec(`INSERT INTO workflow_pr_checks(attempt_id,pr_url,provider,repo,base_branch,state,head_sha,base_sha,error,checked_at)
 SELECT ?,?,p.provider,p.repo,p.base_branch,?,'old-head','old-base','old-error',CASE WHEN ?='checking' THEN 0 ELSE 1 END FROM projects p WHERE p.id=?`, a.ID, a.PRURL, state, state, project)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetCardArchived(c.ID, true); err != nil {
		t.Fatal(err)
	}
	return a
}

// Snapshot every persisted field, not just the UI-visible evidence.
func conflictEvidenceSnapshot(t *testing.T, s *Store, id string) string {
	t.Helper()
	var snapshot string
	err := s.db.QueryRow(`SELECT json_array(attempt_id,pr_url,provider,repo,base_branch,state,head_sha,base_sha,error,checked_at) FROM workflow_pr_checks WHERE attempt_id=?`, id).Scan(&snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestConflictMonitorArchivedAndRestore(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	var project string
	if err := s.db.QueryRow(`SELECT project_id FROM cards WHERE id=?`, a.CardID).Scan(&project); err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s}
	archived := []Attempt{}
	before := map[string]string{}
	for i, state := range []string{"", "conflict", "checking"} {
		extra := addArchivedConflictAttempt(t, s, a, project, fmt.Sprintf("archived%d", i), state)
		archived = append(archived, extra)
		before[extra.ID] = conflictEvidenceSnapshot(t, s, extra.ID)
	}
	calls := []string{}
	w.SetProviderChecks(nil, func(_ context.Context, got Attempt) (PRCheck, error) {
		calls = append(calls, got.ID)
		return PRCheck{State: "clear"}, nil
	})
	if err := w.PollPRConflicts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != a.ID {
		t.Fatalf("calls %v", calls)
	}
	for _, extra := range archived {
		if got := conflictEvidenceSnapshot(t, s, extra.ID); got != before[extra.ID] {
			t.Fatalf("archived evidence changed: %s", got)
		}
	}
	for _, extra := range archived[1:] {
		if err := s.SetCardArchived(extra.CardID, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.PollPRConflicts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("restored calls %v", calls)
	}
}

func TestConflictMonitorSkipsArchivedSelectedAttempt(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	var project string
	if err := s.db.QueryRow(`SELECT project_id FROM cards WHERE id=?`, a.CardID).Scan(&project); err != nil {
		t.Fatal(err)
	}
	later := addArchivedConflictAttempt(t, s, a, project, "later", "conflict")
	if err := s.SetCardArchived(later.CardID, false); err != nil {
		t.Fatal(err)
	}
	before := conflictEvidenceSnapshot(t, s, later.ID)
	calls := 0
	monitor := &conflictMonitor{checker: func(_ context.Context, got Attempt) (PRCheck, error) {
		calls++
		if got.ID != a.ID {
			t.Fatalf("ineligible call %s", got.ID)
		}
		if err := s.SetCardArchived(later.CardID, true); err != nil {
			t.Fatal(err)
		}
		return PRCheck{State: "clear"}, nil
	}}
	w := &Workflow{s: s}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	completed, err := w.checkPRBatch(t.Context(), monitor, []string{a.ID, later.ID})
	if err != nil || completed != 1 || calls != 1 {
		t.Fatalf("completed=%d calls=%d err=%v", completed, calls, err)
	}
	if got := conflictEvidenceSnapshot(t, s, later.ID); got != before {
		t.Fatal("skipped evidence changed")
	}
}
