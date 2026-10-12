package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestRefreshPRChecksForceAndScope(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	var project string
	if err := s.db.QueryRow(`SELECT project_id FROM cards WHERE id=?`, a.CardID).Scan(&project); err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateProjectDetails("other", "github", "https://github.com/acme/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s}
	calls := 0
	w.SetProviderChecks(nil, func(ctx context.Context, got Attempt) (PRCheck, error) {
		calls++
		check, err := w.CardPRCheck(got.CardID)
		if calls > 1 && (err != nil || check.State != "checking" || !check.CheckedAt.IsZero()) {
			t.Fatalf("inflight %+v %v", check, err)
		}
		if _, err := w.RefreshPRChecks(ctx, project, a.CardID); !errors.Is(err, ErrPRRefreshBusy) {
			t.Fatalf("serialization %v", err)
		}
		return PRCheck{State: "clear", HeadSHA: "head", BaseSHA: "base"}, nil
	})
	if err := w.PollPRConflicts(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		res, err := w.RefreshPRChecks(context.Background(), project, a.CardID)
		if err != nil || res != (PRRefreshResult{Targeted: 1, Checked: 1}) {
			t.Fatalf("result %+v %v", res, err)
		}
	}
	if calls != 3 {
		t.Fatal(calls)
	}
	check, err := w.CardPRCheck(a.CardID)
	if err != nil || check.BaseSHA != "base" {
		t.Fatalf("base persistence %+v %v", check, err)
	}
	for _, selection := range [][2]string{{"", a.CardID}, {"missing", a.CardID}, {other.ID, a.CardID}, {project, "missing"}} {
		if _, err := w.RefreshPRChecks(context.Background(), selection[0], selection[1]); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("selection %v: %v", selection, err)
		}
	}
	for _, mutation := range []string{`UPDATE cards SET status='Fix'`, `UPDATE cards SET status='Done'; UPDATE workflow_attempts SET ready=0`, `UPDATE workflow_attempts SET ready=1; INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,created_at,updated_at) SELECT 'next',card_id,2,'next','{}',0,0 FROM workflow_attempts WHERE id='attempt'`} {
		if _, err := s.db.Exec(mutation); err != nil {
			t.Fatal(err)
		}
		if _, err := w.RefreshPRChecks(context.Background(), project, a.CardID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("invalid selection called provider %d", calls)
	}
}

func TestRefreshPRChecksQueuesWholeBoard(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	var project string
	if err := s.db.QueryRow(`SELECT project_id FROM cards WHERE id=?`, a.CardID).Scan(&project); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		card, err := s.CreateCard(project, fmt.Sprint(i), "")
		if err != nil {
			t.Fatal(err)
		}
		extra := a
		extra.ID, extra.TaskID, extra.CardID = fmt.Sprintf("extra%d", i), fmt.Sprintf("extra%d", i), card.ID
		persistConflictAttempt(t, s, extra)
	}
	w := &Workflow{s: s}
	calls := 0
	w.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) { calls++; return PRCheck{State: "clear"}, nil })
	// Fresh checks on every card must not exclude any board-refresh target.
	for range 2 {
		if err := w.PollPRConflicts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	calls = 0
	result, err := w.RefreshPRChecks(context.Background(), project, "")
	if err != nil || result != (PRRefreshResult{Targeted: 21, Checked: 16, Queued: 5}) {
		t.Fatalf("%+v %v", result, err)
	}
	var queued int
	if err := s.db.QueryRow(`SELECT count(*) FROM workflow_pr_checks WHERE state='checking' AND checked_at=0`).Scan(&queued); err != nil || queued != 5 {
		t.Fatalf("queued %d %v", queued, err)
	}
	restarted := &Workflow{s: s}
	restarted.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) { calls++; return PRCheck{State: "clear"}, nil })
	if err := restarted.PollPRConflicts(context.Background()); err != nil || calls != 21 {
		t.Fatalf("eventual %d %v", calls, err)
	}
}

func TestRefreshPRChecksCancellationLeavesQueued(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	var project string
	if err := s.db.QueryRow(`SELECT project_id FROM cards WHERE id=?`, a.CardID).Scan(&project); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &Workflow{s: s}
	w.SetProviderChecks(nil, func(ctx context.Context, _ Attempt) (PRCheck, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded")
		}
		cancel()
		return PRCheck{State: "clear"}, ctx.Err()
	})
	result, err := w.RefreshPRChecks(ctx, project, a.CardID)
	if !errors.Is(err, context.Canceled) || result.Queued != 1 || result.Checked != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	check, err := w.CardPRCheck(a.CardID)
	if err != nil || check.State != "checking" {
		t.Fatalf("%+v %v", check, err)
	}
}
