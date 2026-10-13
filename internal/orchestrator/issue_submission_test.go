package orchestrator

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func issueForm(p Project, token string) url.Values {
	return url.Values{"project_id": {p.ID}, "title": {"  title 日本語  "}, "description": {" # Markdown\nbody "}, "create_issue": {"yes"}, "issue_submission": {token}}
}
func submitDraft(a *app, form url.Values) error {
	r := httptest.NewRequest("POST", "/cards", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.submitIssueCard(r)
}
func TestIssueSubmissionReplayAndRecovery(t *testing.T) {
	for _, scenario := range []string{"success", "rejected", "uncertain", "local failure"} {
		t.Run(scenario, func(t *testing.T) {
			a := testApp(t)
			p, err := a.s.CreateProjectDetails("project", "github", "https://github.com/acme/repo", "master")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("JAWA_GITHUB_TOKEN", "test-token")
			calls := 0
			a.issueClient = &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if scenario == "uncertain" {
					return nil, errors.New("lost response")
				}
				resp := issueResponse(`{"number":17}`)
				resp.StatusCode = 201
				if scenario == "rejected" {
					resp.StatusCode = 403
				}
				return resp, nil
			})}
			if scenario == "local failure" {
				_, err = a.s.db.Exec(`CREATE TRIGGER fail_card BEFORE INSERT ON cards BEGIN SELECT RAISE(FAIL,'fixture'); END`)
				if err != nil {
					t.Fatal(err)
				}
			}
			form := issueForm(p, newID())
			err = submitDraft(a, form)
			if (scenario == "success") != (err == nil) {
				t.Fatalf("initial %v", err)
			}
			if scenario == "local failure" {
				_, _ = a.s.db.Exec(`DROP TRIGGER fail_card`)
				if err = submitDraft(a, form); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "uncertain" || scenario == "rejected" {
				if submitDraft(a, form) == nil {
					t.Fatal("blind retry succeeded")
				}
				form.Del("create_issue")
				form.Set("issue_recovery", "link")
				form.Set("recovery_url", p.Repo+"/issues/17")
				if err = submitDraft(a, form); err != nil {
					t.Fatal(err)
				}
			}
			if err = submitDraft(a, form); err != nil {
				t.Fatal(err)
			}
			cards, err := a.s.Cards(p.ID)
			if err != nil || len(cards) != 1 || cards[0].Title != "title 日本語" || cards[0].Description != form.Get("description") || cards[0].IssueURL != p.Repo+"/issues/17" || calls != 1 {
				t.Fatalf("cards %+v calls %d err %v", cards, calls, err)
			}
			mismatch := issueForm(p, form.Get("issue_submission"))
			mismatch.Set("title", "other")
			if submitDraft(a, mismatch) == nil {
				t.Fatal("payload mismatch accepted")
			}
			if _, err = a.s.db.Exec(`DELETE FROM cards WHERE id=?`, cards[0].ID); err != nil {
				t.Fatal(err)
			}
			if err = submitDraft(a, form); err != nil {
				t.Fatal(err)
			}
			cards, _ = a.s.Cards(p.ID)
			if len(cards) != 0 || calls != 1 {
				t.Fatal("deleted completed replay recreated resources")
			}
		})
	}
}
func TestIssueConcurrentDispatch(t *testing.T) {
	a := testApp(t)
	p, _ := a.s.CreateProjectDetails("project", "github", "https://github.com/acme/repo", "master")
	t.Setenv("JAWA_GITHUB_TOKEN", "fixture")
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	a.issueClient = &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(entered)
		<-release
		resp := issueResponse(`{"number":1}`)
		resp.StatusCode = 201
		return resp, nil
	})}
	form := issueForm(p, newID())
	var wg sync.WaitGroup
	wg.Add(1)
	var first error
	go func() { defer wg.Done(); first = submitDraft(a, form) }()
	<-entered
	if submitDraft(a, form) == nil {
		t.Fatal("busy reservation accepted")
	}
	close(release)
	wg.Wait()
	if first != nil || calls.Load() != 1 {
		t.Fatalf("%v calls %d", first, calls.Load())
	}
}
func TestIssueRestartAndUnlinkedRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateProjectDetails("project", "github", "https://github.com/acme/repo", "master")
	token := newID()
	if _, fresh, err := s.reserveIssueSubmission(token, p, "title", "body"); err != nil || !fresh {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	calls := 0
	a := &app{s: s, issueClient: &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected") })}}
	form := url.Values{"project_id": {p.ID}, "title": {"title"}, "description": {"body"}, "issue_submission": {token}, "create_issue": {"yes"}}
	if submitDraft(a, form) == nil || calls != 0 {
		t.Fatal("restart retried POST")
	}
	form.Del("create_issue")
	form.Set("issue_recovery", "unlinked")
	if err = submitDraft(a, form); err != nil {
		t.Fatal(err)
	}
	if err = submitDraft(a, form); err != nil {
		t.Fatal(err)
	}
	cards, _ := s.Cards(p.ID)
	if len(cards) != 1 || cards[0].IssueURL != "" {
		t.Fatal("unlinked recovery")
	}
}
func TestIssueHTTPValidationAndDraft(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	p, _ := a.s.CreateProjectDetails("project", "github", "https://github.com/acme/repo", "master")
	t.Setenv("JAWA_GITHUB_TOKEN", "fixture")
	calls := 0
	a.issueClient = &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) {
		calls++
		resp := issueResponse("private body")
		resp.StatusCode = 403
		return resp, nil
	})}
	form := issueForm(p, newID())
	form.Set("_csrf", csrf.Value)
	status(t, request(a, "POST", "/cards", form), 303)
	bad := issueForm(p, newID())
	bad.Set("_csrf", "bad")
	status(t, request(a, "POST", "/cards", bad, session, csrf), 403)
	bad.Set("_csrf", csrf.Value)
	bad.Set("issue_url", p.Repo+"/issues/1")
	status(t, request(a, "POST", "/cards", bad, session, csrf), 409)
	if calls != 0 {
		t.Fatal("validation dispatched")
	}
	form.Set("title", "<draft> 日本語")
	w := request(a, "POST", "/cards", form, session, csrf)
	status(t, w, 409)
	if calls != 1 || !strings.Contains(w.Body.String(), "&lt;draft&gt;") || !strings.Contains(w.Body.String(), form.Get("issue_submission")) || strings.Contains(w.Body.String(), "private body") {
		t.Fatal("native draft lost or unsafe")
	}
	r := httptest.NewRequest("POST", "/cards", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Datastar-Request", "true")
	r.AddCookie(session)
	r.AddCookie(csrf)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if calls != 1 || !strings.Contains(w.Body.String(), "card-issue-recovery") || strings.Contains(w.Body.String(), `"cardOpen":false`) {
		t.Fatal("SSE retry or draft closed")
	}
	off := issueForm(p, newID())
	off.Del("create_issue")
	off.Set("_csrf", csrf.Value)
	status(t, request(a, "POST", "/cards", off, session, csrf), 303)
	if calls != 1 {
		t.Fatal("off dispatched")
	}
}

func TestRejectedRetryIsReplaySafe(t *testing.T) {
	a := testApp(t)
	p, _ := a.s.CreateProjectDetails("project", "github", "https://github.com/acme/repo", "master")
	t.Setenv("JAWA_GITHUB_TOKEN", "fixture")
	calls := 0
	a.issueClient = &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) {
		calls++
		resp := issueResponse(`{"number":5}`)
		resp.StatusCode = 201
		if calls == 1 {
			resp.StatusCode = 422
		}
		return resp, nil
	})}
	form := issueForm(p, newID())
	if submitDraft(a, form) == nil {
		t.Fatal("expected rejection")
	}
	form.Del("create_issue")
	form.Set("issue_recovery", "retry")
	form.Set("title", "Corrected title")
	if err := submitDraft(a, form); err != nil {
		t.Fatal(err)
	}
	if err := submitDraft(a, form); err != nil {
		t.Fatal(err)
	}
	cards, _ := a.s.Cards(p.ID)
	if calls != 2 || len(cards) != 1 || cards[0].Title != "Corrected title" {
		t.Fatal("retry replay")
	}
}

func TestNewCardUsesShadcnCreateIssue(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	page := request(a, "GET", "/board", nil, session, csrf)
	for _, markup := range []string{`data-slot="checkbox"`, `role="checkbox"`, `aria-labelledby="card-create-issue-label"`, `aria-describedby="card-create-issue-help"`, `name="create_issue"`, `value="yes"`} {
		if !strings.Contains(page.Body.String(), markup) {
			t.Errorf("New card missing shadcn checkbox markup %s", markup)
		}
	}
}

// Export real server markup/SSE for browser recovery tests without provider mutation.
func TestIssueRecoveryFixture(t *testing.T) {
	path := os.Getenv("JAWA_ISSUE_FIXTURE")
	if path == "" {
		t.Skip("browser fixture export not requested")
	}
	a := testApp(t)
	session, csrf := setupTest(t, a)
	p, _ := a.s.CreateProjectDetails("Fixture project", "github", "https://github.com/acme/repo", "master")
	form := issueForm(p, newID())
	form.Set("_csrf", csrf.Value)
	_, _, err := a.s.reserveIssueSubmission(form.Get("issue_submission"), p, strings.TrimSpace(form.Get("title")), form.Get("description"))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.s.recordIssueOutcome(form.Get("issue_submission"), "uncertain", projectIssue{}); err != nil {
		t.Fatal(err)
	}
	page := request(a, "GET", "/board?project="+p.ID, nil, session, csrf)
	if err = os.WriteFile(path, page.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/cards", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Datastar-Request", "true")
	r.AddCookie(session)
	r.AddCookie(csrf)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	native := request(a, "POST", "/cards", form, session, csrf)
	status(t, native, 409)
	if err = os.WriteFile(path+".native", native.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path+".sse", w.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}
