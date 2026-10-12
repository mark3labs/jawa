package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func sessionExpiry(t *testing.T, a *app, c *http.Cookie) int64 {
	t.Helper()
	var exp int64
	if err := a.s.db.QueryRow("SELECT expires_at FROM sessions WHERE token_hash=?", tokenHash(c.Value)).Scan(&exp); err != nil {
		t.Fatal(err)
	}
	return exp
}

func assertPersistentCookies(t *testing.T, a *app, session, csrf *http.Cookie, before time.Time) {
	t.Helper()
	for _, c := range []*http.Cookie{session, csrf} {
		if c.MaxAge != 2592000 || c.Expires.Before(before.Add(sessionLifetime).Truncate(time.Second)) || c.Expires.After(time.Now().Add(sessionLifetime)) {
			t.Fatalf("%s lifetime incorrect", c.Name)
		}
		if c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteStrictMode || c.Secure || c.HttpOnly != (c.Name == "session") {
			t.Fatalf("%s security attributes incorrect", c.Name)
		}
	}
	if session.Expires != csrf.Expires || sessionExpiry(t, a, session) != session.Expires.Unix() {
		t.Fatal("cookie/database expiry mismatch")
	}
}

func TestSessionSetupLoginAndCSRFRegeneration(t *testing.T) {
	a := testApp(t)
	before := time.Now()
	session, csrf := setupTest(t, a)
	assertPersistentCookies(t, a, session, csrf, before)
	exp := sessionExpiry(t, a, session)
	for range 2 {
		status(t, request(a, "GET", "/board", nil, session, csrf), 200)
	}
	w := request(a, "GET", "/board", nil, session)
	status(t, w, 200)
	csrf = cookie(t, w, "csrf")
	if csrf.MaxAge != 2592000 || csrf.Expires.Before(time.Now().Add(sessionLifetime-time.Minute)) || csrf.HttpOnly || csrf.Path != "/" || csrf.SameSite != http.SameSiteStrictMode {
		t.Fatal("regenerated CSRF attributes incorrect")
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" {
			t.Fatal("CSRF regeneration reissued session")
		}
	}
	status(t, request(a, "POST", "/projects", url.Values{"_csrf": {csrf.Value}, "name": {"demo"}, "provider": {"github"}, "repo": {"https://github.com/org/repo"}, "base_branch": {"master"}}, session, csrf), 303)
	if sessionExpiry(t, a, session) != exp {
		t.Fatal("activity extended session")
	}
	before = time.Now()
	w = request(a, "POST", "/login", url.Values{"_csrf": {csrf.Value}, "username": {"owner"}, "password": {"long-secure-password"}}, session, csrf)
	status(t, w, 303)
	old := session
	session, csrf = cookie(t, w, "session"), cookie(t, w, "csrf")
	assertPersistentCookies(t, a, session, csrf, before)
	r := httptest.NewRequest("GET", "/board", nil)
	r.AddCookie(old)
	if a.authed(r) {
		t.Fatal("old token accepted after re-login")
	}
	status(t, request(a, "POST", "/logout", nil, session, csrf), 403)
	status(t, request(a, "GET", "/board", nil, session), 200)
	w = request(a, "POST", "/logout", url.Values{"_csrf": {csrf.Value}}, session, csrf)
	status(t, w, 303)
	if cookie(t, w, "session").MaxAge != -1 {
		t.Fatal("logout did not expire cookie")
	}
	r = httptest.NewRequest("GET", "/board", nil)
	r.AddCookie(session)
	if a.authed(r) {
		t.Fatal("revoked token accepted")
	}
}

func TestSessionStoreRestartAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{s: s}
	w := httptest.NewRecorder()
	if err := a.issue(w, httptest.NewRequest("POST", "/login", nil)); err != nil {
		if closeErr := s.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	session := cookie(t, w, "session")
	exp := sessionExpiry(t, a, session)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	a = &app{s: s}
	r := httptest.NewRequest("GET", "/board", nil)
	if a.authed(r) {
		t.Fatal("missing token accepted")
	}
	r.AddCookie(&http.Cookie{Name: "session", Value: "unknown"})
	if a.authed(r) {
		t.Fatal("unknown token accepted")
	}
	r = httptest.NewRequest("GET", "/board", nil)
	r.AddCookie(session)
	if !a.authed(r) || sessionExpiry(t, a, session) != exp {
		t.Fatal("session lost across store restart")
	}
	for _, delta := range []time.Duration{48 * time.Hour, 0, -time.Hour} {
		expiry := time.Now().Add(delta).Unix()
		if _, err := s.db.Exec("UPDATE sessions SET expires_at=? WHERE token_hash=?", expiry, tokenHash(session.Value)); err != nil {
			t.Fatal(err)
		}
		if a.authed(r) != (delta > 0) {
			t.Fatalf("incorrect authentication at expiry offset %s", delta)
		}
	}
}
