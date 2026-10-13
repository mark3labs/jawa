package orchestrator

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
)

func TestNATSOnboarding(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, csrf := setupAccountTest(t, a)
	assertReady := func(want bool) {
		t.Helper()
		got, err := a.s.NATSConfigured()
		if err != nil || got != want {
			t.Fatalf("configured = %v, %v; want %v", got, err, want)
		}
	}
	assertReady(false)
	for _, path := range []string{"/", "/board", "/agents"} {
		w := request(a, "GET", path, nil, session, csrf)
		status(t, w, 303)
		if w.Header().Get("Location") != "/settings" {
			t.Fatal("setup bypass: ", w.Header())
		}
	}
	w := request(a, "GET", "/settings", nil, session, csrf)
	status(t, w, 200)
	for _, text := range []string{"Complete NATS authentication setup", `value="worker"`, "Save credentials and continue"} {
		if !strings.Contains(w.Body.String(), text) {
			t.Fatalf("missing onboarding UI %q", text)
		}
	}
	for _, path := range []string{"/projects", "/cards", "/cards/move", "/cards/plan"} {
		form := url.Values{"_csrf": {csrf.Value}, "name": {"blocked"}, "title": {"blocked"}, "repo": {"https://github.com/org/repo"}, "provider": {"github"}}
		w = request(a, "POST", path, form, session, csrf)
		status(t, w, 409)
		if !strings.Contains(w.Body.String(), "Open Settings") {
			t.Fatal("missing setup guidance")
		}
		r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Datastar-Request", "true")
		r.AddCookie(session)
		r.AddCookie(csrf)
		w = httptest.NewRecorder()
		a.ServeHTTP(w, r)
		status(t, w, 200)
		if !strings.Contains(w.Body.String(), `role="alert"`) || !strings.Contains(w.Body.String(), "Open Settings") {
			t.Fatal("missing SSE alert")
		}
	}
	projects, err := a.s.Projects()
	if err != nil || len(projects) != 0 {
		t.Fatalf("blocked project persisted: %v %v", projects, err)
	}
	form := url.Values{"_csrf": {csrf.Value}, "username": {"worker"}, "password": {"short"}, "url": {a.s.NATSConfig().URL}}
	status(t, request(a, "POST", "/settings/nats", form, session, csrf), 400)
	assertReady(false)
	form.Set("password", "long-agent-password")
	status(t, request(a, "POST", "/settings/nats", form, session), 403)
	assertReady(false)
	w = request(a, "POST", "/settings/nats", form, session, csrf)
	status(t, w, 303)
	if w.Header().Get("Location") != "/board" {
		t.Fatal("not continued to board")
	}
	assertReady(true)
	conn, err := nats.Connect(a.ns.ClientURL(), nats.UserInfo("worker", "long-agent-password"))
	if err != nil {
		t.Fatal("saved credentials not applied: ", err)
	}
	conn.Close()
	// Rebuilding listener options must not reset the persisted completion marker.
	if _, err := natsOptions(a.s, "127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	assertReady(true)
	status(t, request(a, "GET", "/board", nil, session, csrf), 200)
	status(t, request(a, "POST", "/projects", url.Values{"_csrf": {csrf.Value}, "name": {"allowed"}, "provider": {"github"}, "repo": {"https://github.com/org/repo"}}, session, csrf), 303)
	// Legacy databases with stored credentials but no completion marker require confirmation.
	if _, err := a.s.db.Exec("DELETE FROM settings WHERE key='nats_configured'"); err != nil {
		t.Fatal(err)
	}
	assertReady(false)
	status(t, request(a, "GET", "/board", nil, session, csrf), 303)
}
