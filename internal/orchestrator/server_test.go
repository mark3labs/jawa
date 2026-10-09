package orchestrator

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"golang.org/x/crypto/bcrypt"
)

func testApp(t *testing.T) *app {
	t.Helper()
	s, e := OpenStore(":memory:")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return &app{s: s}
}
func request(a *app, method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
func cookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("missing cookie %s", name)
	return nil
}
func status(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status %d want %d: %s", w.Code, want, w.Body.String())
	}
}
func setupTest(t *testing.T, a *app) (*http.Cookie, *http.Cookie) {
	t.Helper()
	w := request(a, "GET", "/setup", nil)
	status(t, w, 200)
	c := cookie(t, w, "csrf")
	if !strings.Contains(w.Body.String(), c.Value) {
		t.Fatal("CSRF missing from form")
	}
	w = request(a, "POST", "/setup", url.Values{"_csrf": {c.Value}, "username": {"owner"}, "password": {"long-secure-password"}}, c)
	status(t, w, 303)
	return cookie(t, w, "session"), cookie(t, w, "csrf")
}
func TestAuthCSRFAndRoutes(t *testing.T) {
	a := testApp(t)
	status(t, request(a, "GET", "/login", nil), 303)
	status(t, request(a, "POST", "/setup", url.Values{"password": {"long-secure-password"}}), 403)
	session, csrf := setupTest(t, a)
	if !session.HttpOnly || session.SameSite != http.SameSiteStrictMode {
		t.Fatal("insecure session cookie")
	}
	status(t, request(a, "POST", "/setup", nil), 409)
	status(t, request(a, "GET", "/", nil, session, csrf), 200)
	status(t, request(a, "POST", "/projects", url.Values{"name": {"bad"}}, session), 403)
	status(t, request(a, "GET", "/projects", nil, session), 405)
	status(t, request(a, "POST", "/projects", url.Values{"_csrf": {csrf.Value}, "name": {"demo"}, "provider": {"github"}, "repo": {"https://github.com/org/repo"}, "base_branch": {"main"}}, session, csrf), 303)
	ps, e := a.s.Projects()
	if e != nil || len(ps) != 1 || ps[0].BaseBranch != "main" {
		t.Fatalf("projects: %v %v", ps, e)
	}
	status(t, request(a, "POST", "/cards", url.Values{"_csrf": {csrf.Value}, "project_id": {ps[0].ID}, "title": {"task"}, "description": {"details"}, "issue_url": {"https://example.com/issue"}}, session, csrf), 303)
	cards, e := a.s.Cards("")
	if e != nil || len(cards) != 1 || cards[0].IssueURL == "" {
		t.Fatalf("cards: %v %v", cards, e)
	}
	for _, bad := range []string{"-1", "1junk", "1.5", " 1", "+1", "999999999999999999999999"} {
		status(t, request(a, "POST", "/cards/move", url.Values{"_csrf": {csrf.Value}, "card_id": {cards[0].ID}, "status": {"Building"}, "position": {bad}}, session, csrf), 400)
	}
	status(t, request(a, "POST", "/cards/move", url.Values{"_csrf": {csrf.Value}, "card_id": {cards[0].ID}, "status": {"Building"}, "position": {"0"}}, session, csrf), 303)
	cards, _ = a.s.Cards("")
	if cards[0].Status != "Building" {
		t.Fatal("card not moved")
	}
	status(t, request(a, "POST", "/logout", url.Values{"_csrf": {csrf.Value}}, session, csrf), 303)
	status(t, request(a, "GET", "/", nil, session), 303)
	w := request(a, "GET", "/login", nil)
	c := cookie(t, w, "csrf")
	if !strings.Contains(w.Body.String(), c.Value) {
		t.Fatal("login form missing CSRF")
	}
	status(t, request(a, "POST", "/login", url.Values{"password": {"long-secure-password"}}), 403)
	for _, user := range []string{"wrong", "owner"} {
		p := "incorrect"
		if user == "wrong" {
			p = "long-secure-password"
		}
		w = request(a, "POST", "/login", url.Values{"_csrf": {c.Value}, "username": {user}, "password": {p}}, c)
		status(t, w, 401)
		if !strings.Contains(w.Body.String(), "Invalid username or password") {
			t.Fatal("not generic")
		}
	}
	w = request(a, "POST", "/login", url.Values{"_csrf": {c.Value}, "username": {"owner"}, "password": {"long-secure-password"}}, c)
	status(t, w, 303)
	for range 2 {
		status(t, request(a, "POST", "/login", url.Values{"_csrf": {c.Value}}, c), 401)
	}
	status(t, request(a, "POST", "/login", url.Values{"_csrf": {c.Value}}, c), 429)
}
func TestSecureCookiesBodyLimitAndAssets(t *testing.T) {
	a := testApp(t)
	r := httptest.NewRequest("GET", "https://example.com/setup", nil)
	r.TLS = &tls.ConnectionState{}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if !cookie(t, w, "csrf").Secure {
		t.Fatal("CSRF cookie not secure")
	}
	w = httptest.NewRecorder()
	if e := a.issue(w, r); e != nil {
		t.Fatal(e)
	}
	if !cookie(t, w, "session").Secure {
		t.Fatal("session not secure")
	}
	status(t, request(a, "POST", "/setup", url.Values{"password": {strings.Repeat("x", maxRequestBody+1)}}), 413)
	status(t, request(a, "GET", "/assets/style.css", nil), 200)
	for _, flag := range []string{"data-dir", "listen", "nats-listen"} {
		if Command().Flags().Lookup(flag) == nil {
			t.Fatalf("missing flag %s", flag)
		}
	}
}
func TestNATSAuthenticationAndRotation(t *testing.T) {
	a := testApp(t)
	dir := t.TempDir()
	opts, e := natsOptions(a.s, "127.0.0.1:0", dir)
	if e != nil {
		t.Fatal(e)
	}
	if opts.Username == "" || opts.Password == "" {
		t.Fatal("missing initial credentials")
	}
	if _, e = bcrypt.Cost([]byte(opts.Password)); e != nil {
		t.Fatal(e)
	}
	persisted, e := natsOptions(a.s, "127.0.0.1:0", dir)
	if e != nil || persisted.Password != opts.Password {
		t.Fatal("initial hash not persisted")
	}
	ns, e := server.NewServer(opts)
	if e != nil {
		t.Fatal(e)
	}
	go ns.Start()
	t.Cleanup(func() { ns.Shutdown(); ns.WaitForShutdown() })
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS not ready")
	}
	a.ns = ns
	a.options = opts.Clone()
	connect := func(user, pass string) (*nats.Conn, error) {
		return nats.Connect(ns.ClientURL(), nats.UserInfo(user, pass), nats.NoReconnect(), nats.Timeout(time.Second))
	}
	if nc, e := nats.Connect(ns.ClientURL(), nats.NoReconnect(), nats.Timeout(time.Second)); e == nil {
		nc.Close()
		t.Fatal("anonymous NATS accepted")
	}
	session, csrf := setupTest(t, a)
	rotate := func(user, pass string) {
		t.Helper()
		status(t, request(a, "POST", "/settings/nats", url.Values{"_csrf": {csrf.Value}, "url": {ns.ClientURL()}, "username": {user}, "password": {pass}}, session, csrf), 303)
	}
	rotate("worker", "first-long-secret")
	nc, e := connect("worker", "first-long-secret")
	if e != nil {
		t.Fatal(e)
	}
	nc.Close()
	rotate("worker2", "second-long-secret")
	if nc, e = connect("worker", "first-long-secret"); e == nil {
		nc.Close()
		t.Fatal("old credentials accepted")
	}
	nc, e = connect("worker2", "second-long-secret")
	if e != nil {
		t.Fatal(e)
	}
	defer nc.Close()
	if e = nc.Publish("test.secure", []byte("ok")); e != nil {
		t.Fatal(e)
	}
	if e = nc.Flush(); e != nil {
		t.Fatal(e)
	}
	if nc, e = connect("", ""); e == nil {
		nc.Close()
		t.Fatal("anonymous accepted after rotation")
	}
	var hash string
	if e = a.s.db.QueryRow("SELECT value FROM settings WHERE key='nats_password'").Scan(&hash); e != nil {
		t.Fatal(e)
	}
	if hash == "second-long-secret" || bcrypt.CompareHashAndPassword([]byte(hash), []byte("second-long-secret")) != nil {
		t.Fatal("password not stored as bcrypt")
	}
}
