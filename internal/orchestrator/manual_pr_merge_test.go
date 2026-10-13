package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestManualPRMergePersistence(t *testing.T) {
	t.Parallel()
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	original := repairDone(t, w, c)
	before, _ := w.s.Cards(c.ProjectID)
	if err := w.MarkPRMerged(context.Background(), c.ID, original.ID, original.PRURL); err != nil {
		t.Fatal(err)
	}
	first, err := w.CardPRCheck(c.ID)
	if err != nil || !first.Manual || first.State != "merged" || first.MarkedAt.IsZero() || !first.CheckedAt.IsZero() || first.HeadSHA != "" {
		t.Fatalf("%+v %v", first, err)
	}
	if err := w.MarkPRMerged(context.Background(), c.ID, original.ID, original.PRURL); err != nil {
		t.Fatal(err)
	}
	second, _ := (&Workflow{s: w.s}).CardPRCheck(c.ID)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("replay changed marker")
	}
	after, _ := w.s.Cards(c.ProjectID)
	latest, _ := w.CardResult(c.ID)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(original, latest) {
		t.Fatal("workflow history or card changed")
	}
	for _, state := range []string{"checking", "clear", "conflict", "unknown", "error", "closed", "merged"} {
		w.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) { return PRCheck{State: state}, nil })
		if _, err := w.RefreshPRChecks(context.Background(), c.ProjectID, c.ID); err != nil {
			t.Fatal(err)
		}
		got, err := w.CardPRCheck(c.ID)
		if err != nil || !reflect.DeepEqual(first, got) {
			t.Fatalf("override lost for %s: %+v %v", state, got, err)
		}
	}
	for _, archived := range []bool{true, false} {
		if err := w.s.SetCardArchived(c.ID, archived); err != nil {
			t.Fatal(err)
		}
		got, err := w.CardPRCheck(c.ID)
		if err != nil || !got.Manual {
			t.Fatal("archive lost marker")
		}
	}
	if _, err := w.s.db.Exec(`UPDATE projects SET base_branch='changed' WHERE id=?`, c.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CardPRCheck(c.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("config inherited override", err)
	}
	if err := w.MarkPRMerged(context.Background(), c.ID, original.ID, original.PRURL); err != nil {
		t.Fatal(err)
	}
	if err := w.FixCard(c.ID, "repair"); err != nil {
		t.Fatal(err)
	}
	if err := w.MarkPRMerged(context.Background(), c.ID, original.ID, original.PRURL); err == nil {
		t.Fatal("stale repair accepted")
	}
	if _, err := w.CardPRCheck(c.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("repair inherited marker")
	}
	if _, err := w.s.db.Exec(`DELETE FROM cards WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := w.s.db.QueryRow(`SELECT count(*) FROM workflow_pr_merge_overrides`).Scan(&count); err != nil || count != 0 {
		t.Fatal("cascade failed", err, count)
	}
}

func TestManualPRMergeRejectsInvalid(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"lane", "not ready", "no PR", "active", "attempt", "URL", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			w := actionWorkflow(t)
			c := workflowCard(t, w.s)
			a := repairDone(t, w, c)
			ctx := context.Background()
			var err error
			switch change {
			case "lane":
				_, err = w.s.db.Exec(`UPDATE cards SET status='Todo' WHERE id=?`, c.ID)
			case "not ready":
				_, err = w.s.db.Exec(`UPDATE workflow_attempts SET ready=0 WHERE id=?`, a.ID)
			case "no PR":
				_, err = w.s.db.Exec(`UPDATE workflow_attempts SET pr_url='' WHERE id=?`, a.ID)
			case "active":
				_, err = w.s.db.Exec(`UPDATE workflow_attempts SET run_state='running' WHERE id=?`, a.ID)
			case "attempt":
				a.ID = "stale"
			case "URL":
				a.PRURL = "stale"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = w.MarkPRMerged(ctx, c.ID, a.ID, a.PRURL); err == nil {
				t.Fatal("invalid marker accepted")
			}
			var count int
			if err = w.s.db.QueryRow(`SELECT count(*) FROM workflow_pr_merge_overrides`).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial write", err, count)
			}
		})
	}
}

func TestManualPRMergeDuringProviderIO(t *testing.T) {
	t.Parallel()
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "poll", true: "refresh"}[refresh], func(t *testing.T) {
			w := actionWorkflow(t)
			c := workflowCard(t, w.s)
			a := repairDone(t, w, c)
			entered, release := make(chan struct{}), make(chan struct{})
			w.SetProviderChecks(nil, func(ctx context.Context, _ Attempt) (PRCheck, error) {
				close(entered)
				select {
				case <-release:
					return PRCheck{State: "conflict"}, nil
				case <-ctx.Done():
					return PRCheck{}, ctx.Err()
				}
			})
			done := make(chan error, 1)
			go func() {
				if refresh {
					_, err := w.RefreshPRChecks(context.Background(), c.ProjectID, c.ID)
					done <- err
				} else {
					done <- w.PollPRConflicts(context.Background())
				}
			}()
			<-entered
			err := w.MarkPRMerged(context.Background(), c.ID, a.ID, a.PRURL)
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			got, err := w.CardPRCheck(c.ID)
			if err != nil || !got.Manual || got.State != "merged" {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestMarkMergedRoute(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, csrf := setupTest(t, a)
	c := workflowCard(t, a.s)
	attempt := repairDone(t, a.workflow, c)
	form := url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}, "expected_attempt_id": {attempt.ID}, "expected_pr_url": {attempt.PRURL}, "view": {"board"}, "project": {c.ProjectID}}
	status(t, request(a, "GET", "/cards/mark-merged", nil, session, csrf), http.StatusMethodNotAllowed)
	status(t, request(a, "POST", "/cards/mark-merged", nil, session, csrf), http.StatusForbidden)
	status(t, request(a, "POST", "/cards/mark-merged", form), http.StatusSeeOther)
	native := request(a, "POST", "/cards/mark-merged", form, session, csrf)
	status(t, native, http.StatusSeeOther)
	if native.Header().Get("Location") != boardPath(c.ProjectID) {
		t.Fatal("lost view")
	}
	form.Set("expected_attempt_id", "stale")
	status(t, request(a, "POST", "/cards/mark-merged", form, session, csrf), http.StatusBadRequest)
	form.Set("expected_attempt_id", attempt.ID)
	body := assertSSE(t, sseRequest(a, "POST", "/cards/mark-merged", form, session, csrf))
	if !strings.Contains(body, "Marked as merged manually") || strings.Contains(body, "Last checked") || strings.Contains(body, "Mark as merged</span>") {
		t.Fatal("missing provenance or stale action")
	}
	if strings.Count(body, ">Merged</span>") != 1 {
		t.Fatal("expected single merged chip")
	}
	a.workflow.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) { return PRCheck{State: "clear"}, nil })
	if _, err := a.workflow.RefreshPRChecks(context.Background(), c.ProjectID, c.ID); err != nil {
		t.Fatal(err)
	}
	check, _ := a.workflow.CardPRCheck(c.ID)
	if !check.Manual {
		t.Fatal("refresh lost override")
	}
}

func TestManualPRMergeStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s}
	c := workflowCard(t, s)
	// Use the existing immutable assignment fixture with this persistent card.
	_, a := conflictFixture(t, "github")
	a.CardID = c.ID
	persistConflictAttempt(t, s, a)
	if err = w.MarkPRMerged(context.Background(), c.ID, a.ID, a.PRURL); err != nil {
		t.Fatal(err)
	}
	before, _ := w.CardPRCheck(c.ID)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	after, err := (&Workflow{s: s}).CardPRCheck(c.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("restart: %+v %v", after, err)
	}
}

func TestManualPRMergeRacingFix(t *testing.T) {
	t.Parallel()
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	a := repairDone(t, w, c)
	start := make(chan struct{})
	done := make(chan error, 2)
	go func() { <-start; done <- w.MarkPRMerged(context.Background(), c.ID, a.ID, a.PRURL) }()
	go func() { <-start; done <- w.FixCard(c.ID, "repair") }()
	close(start)
	<-done
	<-done
	latest, err := w.CardResult(c.ID)
	if err != nil || latest.ID == a.ID {
		t.Fatal("repair not queued", err)
	}
	var count int
	if err = w.s.db.QueryRow(`SELECT count(*) FROM workflow_pr_merge_overrides WHERE attempt_id=?`, latest.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("marker attached to replacement", err)
	}
	if _, err = w.CardPRCheck(c.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("replacement inherited marker", err)
	}
}
