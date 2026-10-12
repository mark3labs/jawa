package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPRRefreshRoute(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	c := workflowCard(t, a.s)
	repairDone(t, a.workflow, c)
	var calls atomic.Int32
	a.workflow.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) {
		calls.Add(1)
		return PRCheck{State: "clear", HeadSHA: "head-evidence", BaseSHA: "base-evidence"}, nil
	})
	form := url.Values{"_csrf": {csrf.Value}, "project_id": {c.ProjectID}, "card_id": {c.ID}, "view": {"board"}, "project": {c.ProjectID}}
	status(t, request(a, "GET", "/prs/refresh", nil, session, csrf), http.StatusMethodNotAllowed)
	status(t, request(a, "POST", "/prs/refresh", nil, session, csrf), http.StatusForbidden)
	status(t, request(a, "POST", "/prs/refresh", form), http.StatusSeeOther)
	if calls.Load() != 0 {
		t.Fatal("unguarded refresh")
	}
	for _, selection := range [][2]string{{"", c.ID}, {"missing", c.ID}, {c.ProjectID, "missing"}} {
		invalid := url.Values{}
		for k, v := range form {
			invalid[k] = append([]string(nil), v...)
		}
		invalid.Set("project_id", selection[0])
		invalid.Set("card_id", selection[1])
		status(t, request(a, "POST", "/prs/refresh", invalid, session, csrf), http.StatusNotFound)
	}
	native := request(a, "POST", "/prs/refresh", form, session, csrf)
	status(t, native, http.StatusSeeOther)
	if native.Header().Get("Location") != boardPath(c.ProjectID) {
		t.Fatal("lost board selection")
	}
	body := assertSSE(t, sseRequest(a, "POST", "/prs/refresh", form, session, csrf))
	for _, want := range []string{"1 targeted, 1 checked, 0 queued", `data-pr-check-state="clear"`, "Last checked", "head-evidence", "base-evidence", "Check PR now", "Refresh PR status"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("manual check did not bypass cooldown")
	}
	form.Set("card_id", "")
	status(t, request(a, "POST", "/prs/refresh", form, session, csrf), http.StatusSeeOther)
	a.workflow.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) {
		return PRCheck{}, errors.New("synthetic secret transport failure")
	})
	body = assertSSE(t, sseRequest(a, "POST", "/prs/refresh", form, session, csrf))
	if !strings.Contains(body, `data-pr-check-state="error"`) || strings.Contains(body, "No merge conflicts detected") || strings.Contains(body, "synthetic secret") {
		t.Fatal("failure rendered false clear or leaked provider error")
	}
	a.workflow.SetProviderChecks(nil, nil)
	status(t, request(a, "POST", "/prs/refresh", form, session, csrf), http.StatusServiceUnavailable)
	body = assertSSE(t, sseRequest(a, "POST", "/prs/refresh", form, session, csrf))
	if !strings.Contains(body, "PR checks are unavailable") || !strings.Contains(body, `data-pr-check-state="error"`) {
		t.Fatal("unavailable lost evidence")
	}
	a.workflow = nil
	status(t, request(a, "POST", "/prs/refresh", form, session, csrf), http.StatusServiceUnavailable)
}

func TestPRRefreshBusyAndCancelled(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	c := workflowCard(t, a.s)
	repairDone(t, a.workflow, c)
	entered, release := make(chan struct{}), make(chan struct{})
	a.workflow.SetProviderChecks(nil, func(ctx context.Context, _ Attempt) (PRCheck, error) {
		close(entered)
		select {
		case <-release:
			return PRCheck{State: "clear"}, nil
		case <-ctx.Done():
			return PRCheck{}, ctx.Err()
		}
	})
	done := make(chan error, 1)
	go func() { _, err := a.workflow.RefreshPRChecks(t.Context(), c.ProjectID, c.ID); done <- err }()
	<-entered
	form := url.Values{"_csrf": {csrf.Value}, "project_id": {c.ProjectID}, "card_id": {c.ID}, "view": {"board"}, "project": {c.ProjectID}}
	status(t, request(a, "POST", "/prs/refresh", form, session, csrf), http.StatusConflict)
	body := assertSSE(t, sseRequest(a, "POST", "/prs/refresh", form, session, csrf))
	if !strings.Contains(body, "already running") || !strings.Contains(body, `data-pr-check-state="checking"`) || strings.Contains(body, "No merge conflicts detected") {
		t.Fatal("busy response lost checking state")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	a.workflow.SetProviderChecks(nil, func(ctx context.Context, _ Attempt) (PRCheck, error) { <-ctx.Done(); return PRCheck{}, ctx.Err() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	// Route-level cancellation must leave fresh checking evidence, not stale clear.
	r := httptest.NewRequest("POST", "/prs/refresh", strings.NewReader(form.Encode())).WithContext(ctx)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(session)
	r.AddCookie(csrf)
	a.ServeHTTP(httptest.NewRecorder(), r)
	check, err := a.workflow.CardPRCheck(c.ID)
	if err != nil || check.State != "checking" || !check.CheckedAt.IsZero() {
		t.Fatalf("check %+v: %v", check, err)
	}
}

func TestPRCheckPresentation(t *testing.T) {
	c := Card{ID: "done", ProjectID: "project", Status: "Done"}
	attempts := []Attempt{{CardID: c.ID, Number: 1, State: "ready", Ready: true, PRURL: "https://github.com/example/repo/pull/18"}}
	labels := map[string]string{"checking": "Checking PR status", "clear": "No merge conflicts detected", "conflict": "Pull request has merge conflicts.", "unknown": "PR merge status is unknown", "error": "PR check failed", "merged": "Pull request is merged", "closed": "closed without merging", "": "not been checked yet"}
	for state, label := range labels {
		t.Run(state, func(t *testing.T) {
			check := PRCheck{State: state}
			if state != "checking" && state != "" {
				check.CheckedAt = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
			}
			d := pageData{uiSnapshot: uiSnapshot{attempts: attempts, prChecks: map[string]PRCheck{c.ID: check}}}
			var out bytes.Buffer
			if err := cardNote(d, c).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			body := out.String()
			if !strings.Contains(body, label) || !strings.Contains(body, "Check PR now") {
				t.Fatal(body)
			}
			if strings.Contains(body, "Last checked") != !check.CheckedAt.IsZero() {
				t.Fatal("invalid timestamp")
			}
			if state == "checking" && !strings.Contains(body, "Awaiting a fresh check") {
				t.Fatal("checking implied prior evidence")
			}
		})
	}
	for _, card := range []Card{{ID: c.ID, Status: "Fix"}, {ID: "legacy", Status: "Done"}} {
		var out bytes.Buffer
		d := pageData{uiSnapshot: uiSnapshot{attempts: attempts}}
		if err := cardNote(d, card).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "Check PR now") {
			t.Fatal("ineligible refresh button")
		}
	}
}

func TestPRRefreshBoardQueueCounts(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	first := workflowCard(t, a.s)
	repairDone(t, a.workflow, first)
	for i := 0; i < 16; i++ {
		c, err := a.s.CreateCard(first.ProjectID, "Another ready PR", "")
		if err != nil {
			t.Fatal(err)
		}
		repairDone(t, a.workflow, c)
	}
	// Install only for this request, so the fixture's background loop cannot
	// consume queued work before the response assertions.
	a.workflow.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) { return PRCheck{State: "unknown"}, nil })
	form := url.Values{"_csrf": {csrf.Value}, "project_id": {first.ProjectID}, "view": {"board"}, "project": {first.ProjectID}}
	body := assertSSE(t, sseRequest(a, "POST", "/prs/refresh", form, session, csrf))
	if !strings.Contains(body, "17 targeted, 16 checked, 1 queued for background checking") || !strings.Contains(body, `data-pr-check-state="checking"`) || !strings.Contains(body, "Awaiting a fresh check.") {
		t.Fatal("queued work missing from response")
	}
}
