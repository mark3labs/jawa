package orchestrator

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mergeFixture(t *testing.T) (*Workflow, Card, Attempt, string) {
	t.Helper()
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	w := &Workflow{s: s, ctx: t.Context()}
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	w.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) {
		return PRCheck{State: "clear", HeadSHA: head, BaseSHA: base}, nil
	})
	if err := w.PollPRConflicts(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, p, _, _, err := readySnapshot(s.db, a.CardID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := w.Attempts()
	if err != nil {
		t.Fatal(err)
	}
	a = latestAttempt(c.ID, all)[0]
	return w, c, a, mergeSnapshot(c, a, p, head, base)
}

func TestMergeReservationAndReplay(t *testing.T) {
	for _, result := range []string{"merged", "rejected", "uncertain"} {
		t.Run(result, func(t *testing.T) {
			w, c, a, snapshot := mergeFixture(t)
			var calls atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			w.SetProviderMerger(func(ctx context.Context, got Attempt, head, method string) string {
				calls.Add(1)
				close(entered)
				<-release
				if got.ID != a.ID || head != strings.Repeat("a", 40) || method != "squash" {
					t.Error("bad assignment")
				}
				return result
			})
			op := strings.Repeat("c", 32)
			done := make(chan error, 1)
			go func() { done <- w.MergePR(t.Context(), c.ID, op, snapshot, "squash", "operator") }()
			<-entered
			for _, action := range []func() error{
				func() error { return w.MergePR(t.Context(), c.ID, op, snapshot, "squash", "operator") },
				func() error { return w.MergePR(t.Context(), c.ID, op, snapshot, "merge", "operator") },
				func() error {
					return w.MergePR(t.Context(), c.ID, strings.Repeat("d", 32), snapshot, "merge", "operator")
				},
				func() error { return w.FixCard(c.ID, "fix") },
				func() error { return w.fixCardTo(c.ID, "fix", "agent") },
				func() error { return w.DeleteCard(c.ID) },
				func() error { return w.ResetCard(c.ID) },
				func() error { return w.s.SetCardArchived(c.ID, true) },
				func() error { return w.MarkPRMerged(t.Context(), c.ID, a.ID, a.PRURL) },
			} {
				if err := action(); err == nil {
					t.Error("admitted concurrent action")
				}
			}
			check, err := w.CardPRCheck(c.ID)
			if err != nil || check.State != "pending" {
				t.Fatal(check, err)
			}
			close(release)
			err = <-done
			if (result == "merged") != (err == nil) {
				t.Fatal("wrong outcome", err)
			}
			err = w.MergePR(t.Context(), c.ID, op, snapshot, "squash", "operator")
			if (result == "merged") != (err == nil) || calls.Load() != 1 {
				t.Fatal("replay wrote again")
			}
			latest, _ := w.CardResult(c.ID)
			if !latest.Ready || latest.State != "ready" {
				t.Fatal("changed readiness")
			}
			cards, _ := w.s.Cards(c.ProjectID)
			if cards[0].Status != "Done" || cards[0].Archived {
				t.Fatal("changed card")
			}
			switch result {
			case "merged":
				check, err = w.CardPRCheck(c.ID)
				if err != nil || check.State != "merged" || check.Manual {
					t.Fatal(check, err)
				}
				_, _ = w.RefreshPRChecks(t.Context(), c.ProjectID, "")
				check, _ = w.CardPRCheck(c.ID)
				if check.State != "merged" {
					t.Fatal("refresh clobbered merge")
				}
			case "uncertain":
				// Restart recovery is read-only and waits until the entire request window.
				if err = w.ReconcileMerges(t.Context(), w.conflicts.checker); err != nil {
					t.Fatal(err)
				}
				if err = w.s.SetCardArchived(c.ID, true); err == nil {
					t.Fatal("released too early")
				}
				_, err = w.s.db.Exec(`UPDATE pr_merges SET created_at=?`, time.Now().Add(-time.Minute).UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				if err = w.ReconcileMerges(t.Context(), w.conflicts.checker); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 1 {
					t.Fatal("replayed write")
				}
				if err = w.s.SetCardArchived(c.ID, true); err != nil {
					t.Fatal("no reconciliation escape", err)
				}
			}
		})
	}
}

func TestMergeStalePollAndAdmission(t *testing.T) {
	w, c, _, snapshot := mergeFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	w.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) {
		close(entered)
		<-release
		return PRCheck{State: "clear", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}, nil
	})
	// Background polling doesn't invalidate the rendered clear confirmation.
	_, _ = w.s.db.Exec(`UPDATE workflow_pr_checks SET checked_at=0`)
	done := make(chan error, 1)
	go func() { done <- w.PollPRConflicts(t.Context()) }()
	<-entered
	w.SetProviderMerger(func(context.Context, Attempt, string, string) string { return "merged" })
	if err := w.MergePR(t.Context(), c.ID, strings.Repeat("a", 32), snapshot, "merge", "operator"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	check, _ := w.CardPRCheck(c.ID)
	if check.State != "merged" {
		t.Fatal("stale poll overwrote merge")
	}
}

func TestMergeEligibility(t *testing.T) {
	for _, state := range []string{"checking", "unknown", "error", "conflict", "closed", "merged", "clear"} {
		t.Run(state, func(t *testing.T) {
			w, c, _, snapshot := mergeFixture(t)
			_, err := w.s.db.Exec(`UPDATE workflow_pr_checks SET state=?`, state)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			w.SetProviderMerger(func(context.Context, Attempt, string, string) string { calls++; return "merged" })
			err = w.MergePR(t.Context(), c.ID, strings.Repeat("a", 32), snapshot, "merge", "operator")
			if (state == "clear") != (err == nil) || (state == "clear") != (calls == 1) {
				t.Fatal(state, err, calls)
			}
		})
	}
	for _, change := range []string{"archive", "project", "head", "manual", "stale", "operator", "method"} {
		t.Run(change, func(t *testing.T) {
			w, c, a, snapshot := mergeFixture(t)
			method, operator := "merge", "operator"
			switch change {
			case "archive":
				if err := w.s.SetCardArchived(c.ID, true); err != nil {
					t.Fatal(err)
				}
			case "project":
				_, _ = w.s.db.Exec(`UPDATE projects SET base_branch='other'`)
			case "head":
				_, _ = w.s.db.Exec(`UPDATE workflow_pr_checks SET head_sha=?`, strings.Repeat("c", 40))
			case "manual":
				if err := w.MarkPRMerged(t.Context(), c.ID, a.ID, a.PRURL); err != nil {
					t.Fatal(err)
				}
			case "stale":
				snapshot = "wrong"
			case "operator":
				operator = ""
			case "method":
				method = "force"
			}
			w.SetProviderMerger(func(context.Context, Attempt, string, string) string { t.Error("unexpected write"); return "merged" })
			if err := w.MergePR(t.Context(), c.ID, strings.Repeat("a", 32), snapshot, method, operator); err == nil {
				t.Fatal("accepted stale merge")
			}
		})
	}
}

func TestMergeRoute(t *testing.T) {
	app := testApp(t)
	session, csrf := setupTest(t, app)
	c := workflowCard(t, app.s)
	a := repairDone(t, app.workflow, c)
	c.Status = "Done"
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	app.workflow.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) {
		return PRCheck{State: "clear", HeadSHA: head, BaseSHA: base}, nil
	})
	if _, err := app.workflow.RefreshPRChecks(t.Context(), c.ProjectID, c.ID); err != nil {
		t.Fatal(err)
	}
	_, p, _, _, err := readySnapshot(app.s.db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	app.workflow.SetProviderMerger(func(context.Context, Attempt, string, string) string { calls++; return "merged" })
	form := url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}, "operation": {strings.Repeat("a", 32)}, "dialog_operation": {strings.Repeat("a", 32)}, "snapshot": {mergeSnapshot(c, a, p, head, base)}, "method": {"merge"}, "view": {"board"}, "project": {c.ProjectID}}
	status(t, request(app, "GET", "/cards/merge", nil, session, csrf), http.StatusMethodNotAllowed)
	status(t, request(app, "POST", "/cards/merge", nil, session, csrf), http.StatusForbidden)
	status(t, request(app, "POST", "/cards/merge", form), http.StatusSeeOther)
	if calls != 0 {
		t.Fatal("unguarded merge")
	}
	form.Set("snapshot", "invalid")
	body := assertSSE(t, sseRequest(app, "POST", "/cards/merge", form, session, csrf))
	if !strings.Contains(body, "merge-error-"+strings.Repeat("a", 32)) || calls != 0 {
		t.Fatal("not dialog scoped")
	}
	form.Set("snapshot", mergeSnapshot(c, a, p, head, base))
	response := request(app, "POST", "/cards/merge", form, session, csrf)
	status(t, response, http.StatusSeeOther)
	if response.Header().Get("Location") != boardPath(c.ProjectID) {
		t.Fatal("view lost")
	}
	body = assertSSE(t, sseRequest(app, "POST", "/cards/merge", form, session, csrf))
	if !strings.Contains(body, `data-pr-check-state="merged"`) || !strings.Contains(body, `data-succeeded`) || strings.Contains(body, `"mergeOpen":false`) || calls != 1 {
		t.Fatal("wrong success/replay")
	}
	app.workflow = nil
	status(t, request(app, "POST", "/cards/merge", form, session, csrf), http.StatusBadRequest)
}

func TestMergeUIFixture(t *testing.T) {
	w, c, a, _ := mergeFixture(t)
	projects, err := w.s.Projects()
	if err != nil {
		t.Fatal(err)
	}
	check, err := w.CardPRCheck(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	d := pageData{v: viewParams{View: viewBoard, Project: c.ProjectID}, now: time.Now(), csrf: "fixture-csrf", projects: projects, prChecks: map[string]PRCheck{}, planning: map[string]PlanningDetail{}}
	for _, state := range []string{"clear", "unknown", "checking", "error", "conflict", "closed", "merged", "pending", "uncertain", "manual", "archived"} {
		card := c
		card.ID = state
		card.Title = "Merge fixture: " + state
		card.Description = "Scripted provider evidence only"
		card.Archived = state == "archived"
		attempt := a
		attempt.ID = state
		attempt.CardID = state
		evidence := check
		evidence.AttemptID = state
		switch state {
		case "archived":
			evidence.State = "clear"
		case "manual":
			evidence.State = "merged"
			evidence.Manual = true
		default:
			evidence.State = state
		}
		d.cards = append(d.cards, card)
		d.attempts = append(d.attempts, attempt)
		d.prChecks[state] = evidence
	}
	var out bytes.Buffer
	if err = appPage(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "Merge…") != 1 {
		t.Fatal("wrong eligible button count")
	}
	if path := os.Getenv("JAWA_MERGE_HTML"); path != "" {
		if err = os.WriteFile(path, out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	d.v.View = viewArchive
	out.Reset()
	if err = appPage(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Merge…") {
		t.Fatal("archive merge action")
	}
}

func TestMergeRestartRecovery(t *testing.T) {
	// File-backed migration/restart test: no write is replayed on recovery.
	path := t.TempDir() + "/jawa.db"
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context()}
	c := workflowCard(t, s)
	a := repairDone(t, w, c)
	_, err = s.db.Exec(`INSERT INTO pr_merges(id,card_id,attempt_id,pr_url,snapshot,provider,repo,base_branch,head_sha,method,operator,state,created_at,updated_at) SELECT ?,?,?,?,'snapshot',provider,repo,base_branch,?,'merge','operator','pending',?,0 FROM projects WHERE id=?`, strings.Repeat("a", 32), c.ID, a.ID, a.PRURL, strings.Repeat("a", 40), time.Now().Add(-time.Minute).UnixMilli(), c.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	w = &Workflow{s: s, ctx: t.Context()}
	w.SetProviderMerger(func(context.Context, Attempt, string, string) string { t.Error("replayed write"); return "merged" })
	check, err := w.CardPRCheck(c.ID)
	if err != nil || check.State != "uncertain" {
		t.Fatal(check, err)
	}
	// Unreadable evidence must retain the lock.
	if err = w.ReconcileMerges(t.Context(), func(context.Context, Attempt) (PRCheck, error) { return PRCheck{}, context.DeadlineExceeded }); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCardArchived(c.ID, true); err == nil {
		t.Fatal("lost uncertainty")
	}
	if err = w.ReconcileMerges(t.Context(), func(context.Context, Attempt) (PRCheck, error) {
		return PRCheck{State: "merged", HeadSHA: strings.Repeat("a", 40)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	check, err = w.CardPRCheck(c.ID)
	if err != nil || check.State != "merged" || check.Manual {
		t.Fatal(check, err)
	}
}

// Both entry points must respect the other ledger before dispatching a write.
func TestAutoAndOperatorMergeReservations(t *testing.T) {
	for _, state := range []string{"reserved", "inflight", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			w, c, a, snapshot := mergeFixture(t)
			w.SetProviderMerger(func(context.Context, Attempt, string, string) string {
				t.Error("operator write during unresolved auto merge")
				return "merged"
			})
			_, err := w.s.db.Exec(`INSERT INTO auto_merge_operations(id,card_id,attempt_id,provider,repository,base_branch,pr_url,head_sha,config_token,state,next_check_at,created_at,updated_at) VALUES('auto',?,?,'github','acme/repo','main',?,?,'token',?,0,0,0)`, c.ID, a.ID, a.PRURL, strings.Repeat("a", 40), state)
			if err != nil {
				t.Fatal(err)
			}
			if err = w.MergePR(t.Context(), c.ID, strings.Repeat("c", 32), snapshot, "squash", "operator"); err == nil {
				t.Fatal("operator ignored auto reservation")
			}
			if err = w.MarkPRMerged(t.Context(), c.ID, a.ID, a.PRURL); err == nil {
				t.Fatal("manual marker ignored auto reservation")
			}
		})
	}
	for _, state := range []string{"pending", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			w, c, a, snapshot := mergeFixture(t)
			if err := w.SetCardAutoMerge(c.ID, "inherit", "on"); err != nil {
				t.Fatal(err)
			}
			_, err := w.s.db.Exec(`INSERT INTO pr_merges(id,card_id,attempt_id,pr_url,snapshot,provider,repo,base_branch,head_sha,method,operator,state,created_at,updated_at) VALUES('operator',?,?,?,?, 'github','acme/repo','main',?,'squash','operator',?,0,0)`, c.ID, a.ID, a.PRURL, snapshot, strings.Repeat("a", 40), state)
			if err != nil {
				t.Fatal(err)
			}
			writes := 0
			w.merges = &autoMergeWorker{adapter: mergeTestAdapter(t, w.s, a, "github", "success", &writes)}
			w.autoMergePass(t.Context())
			if writes != 0 {
				t.Fatal("auto merge ignored operator reservation")
			}
		})
	}
}
