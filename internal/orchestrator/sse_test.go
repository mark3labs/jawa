package orchestrator

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func sseCookies(t *testing.T, a *app) (*http.Cookie, *http.Cookie) {
	t.Helper()
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	if err := a.issue(w, r); err != nil {
		t.Fatal(err)
	}
	return cookie(t, w, "session"), cookie(t, w, "csrf")
}

func sseRequest(a *app, method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Datastar-Request", "true")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func assertSSE(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	status(t, w, http.StatusOK)
	if w.Header().Get("Content-Type") != "text/event-stream" || !w.Flushed {
		t.Fatalf("not flushed SSE: %v", w.Header())
	}
	if w.Header().Get("Location") != "" {
		t.Fatal("SSE redirected")
	}
	body := w.Body.String()
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("unterminated frame: %q", body)
	}
	return body
}

func TestSSESnapshotAndAuthentication(t *testing.T) {
	a := &app{s: actionWorkflow(t).s}
	session, csrf := sseCookies(t, a)
	p, err := a.s.CreateProjectDetails("Real project", "github", "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.CreateCardDetails(p.ID, "Real card", "line one\nline two", ""); err != nil {
		t.Fatal(err)
	}
	for _, ds := range []bool{false, true} {
		var w *httptest.ResponseRecorder
		if ds {
			w = sseRequest(a, "GET", "/snapshot", nil, session, csrf)
		} else {
			w = request(a, "GET", "/snapshot", nil, session, csrf)
		}
		body := assertSSE(t, w)
		for _, text := range []string{`id="board-content"`, `id="agent-status"`, "Real project", "Real card", "data: elements "} {
			if !strings.Contains(body, text) {
				t.Fatalf("missing %q: %s", text, body)
			}
		}
		for _, text := range []string{`id="notice"`, `id="project-dialog"`, "datastar-patch-signals", "<html"} {
			if strings.Contains(body, text) {
				t.Fatalf("unsolicited patch contains %q", text)
			}
		}
	}
	for _, path := range []string{"/events", "/snapshot"} {
		w := request(a, "GET", path, nil)
		status(t, w, http.StatusUnauthorized)
		if w.Header().Get("Location") != "" {
			t.Fatal("unauthenticated stream redirected")
		}
	}
	if _, err := a.s.db.Exec("UPDATE sessions SET expires_at=0"); err != nil {
		t.Fatal(err)
	}
	status(t, request(a, "GET", "/snapshot", nil, session), http.StatusUnauthorized)
}

func TestSSEMutationsAndErrors(t *testing.T) {
	a := testApp(t)
	session, csrf := sseCookies(t, a)
	form := url.Values{"_csrf": {csrf.Value}, "name": {"SSE project"}, "provider": {"github"}, "repo": {"https://github.com/example/repo"}, "base_branch": {"main"}}
	body := assertSSE(t, sseRequest(a, "POST", "/projects", form, session, csrf))
	for _, text := range []string{`id="notice"`, `id="board-content"`, `id="agent-status"`, `event: datastar-patch-signals`, `"projectOpen":false`} {
		if !strings.Contains(body, text) {
			t.Fatalf("missing %q", text)
		}
	}
	projects, err := a.s.Projects()
	if err != nil {
		t.Fatal(err)
	}
	form = url.Values{"_csrf": {csrf.Value}, "project_id": {projects[0].ID}, "title": {"SSE card"}}
	body = assertSSE(t, sseRequest(a, "POST", "/cards", form, session, csrf))
	if !strings.Contains(body, `"cardOpen":false`) {
		t.Fatal(body)
	}
	cards, err := a.s.Cards("")
	if err != nil {
		t.Fatal(err)
	}
	id := cards[0].ID
	// Legacy Building cards have no attempts; retry must still be accepted.
	if err := a.s.MoveCard(id, "Building", 0); err != nil {
		t.Fatal(err)
	}
	form = url.Values{"_csrf": {csrf.Value}, "card_id": {id}}
	assertSSE(t, sseRequest(a, "POST", "/cards/retry", form, session, csrf))
	if _, err := a.s.db.Exec("UPDATE workflow_attempts SET state='failed',run_state='failed' WHERE card_id=?", id); err != nil {
		t.Fatal(err)
	}
	assertSSE(t, sseRequest(a, "POST", "/cards/reset", form, session, csrf))
	body = assertSSE(t, sseRequest(a, "POST", "/cards/delete", form, session, csrf))
	if !strings.Contains(body, `"deleteOpen":false`) {
		t.Fatal(body)
	}
	cards, err = a.s.Cards("")
	if err != nil || len(cards) != 0 {
		t.Fatalf("delete: %v %v", cards, err)
	}
	for _, path := range []string{"/cards/move", "/cards/retry", "/cards/reset", "/cards/delete"} {
		body := assertSSE(t, sseRequest(a, "POST", path, form, session, csrf))
		if !strings.Contains(body, `id="notice"`) || strings.Contains(body, "datastar-patch-signals") {
			t.Fatal(body)
		}
		status(t, request(a, "POST", path, form, session, csrf), http.StatusBadRequest)
	}
	// Cancellation errors must dismiss the confirmation dialog so the notice
	// is visible, while not claiming the remote run was stopped.
	body = assertSSE(t, sseRequest(a, "POST", "/cards/cancel", form, session, csrf))
	if !strings.Contains(body, `id="notice"`) || !strings.Contains(body, `"cancelOpen":false`) || !strings.Contains(body, `"cancelCard":""`) {
		t.Fatal(body)
	}
	// Successful mutation snapshots must dismiss it as well.
	cancelResponse := httptest.NewRecorder()
	cancelRequest := httptest.NewRequest("POST", "/cards/cancel", nil)
	cancelRequest.Header.Set("Datastar-Request", "true")
	a.snapshot(cancelResponse, cancelRequest, true, mutationEffects{flash: "Cancellation requested; awaiting confirmation."})
	body = assertSSE(t, cancelResponse)
	if !strings.Contains(body, `"cancelOpen":false`) || !strings.Contains(body, `"cancelCard":""`) || !strings.Contains(body, "awaiting confirmation") {
		t.Fatal(body)
	}
	body = assertSSE(t, sseRequest(a, "POST", "/cards", nil, session, csrf))
	if !strings.Contains(body, "CSRF") {
		t.Fatal(body)
	}
	status(t, request(a, "POST", "/cards", nil, session, csrf), http.StatusForbidden)
	assertSSE(t, sseRequest(a, "PUT", "/cards", nil, session, csrf))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/cards", nil)
	r.Header.Set("Datastar-Request", "true")
	a.responseError(w, r, "<script>bad</script>\nevent: injected\rdata: bad", http.StatusBadRequest)
	body = assertSSE(t, w)
	if strings.Contains(body, "<script>") || strings.Contains(body, "\nevent: injected") || strings.Contains(body, "\r") {
		t.Fatal(body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal(body)
	}
	// Native creation still follows the original Post/Redirect/Get contract.
	status(t, request(a, "POST", "/cards", url.Values{"_csrf": {csrf.Value}, "project_id": {projects[0].ID}, "title": {"Native card"}}, session, csrf), http.StatusSeeOther)
}

// Exercise real streaming, rather than reading a recorder concurrently with a
// handler. EOF proves cancellation/revocation releases the long-lived handler.
func TestSSELiveLifecycle(t *testing.T) {
	for _, end := range []string{"cancel", "revoke", "expire", "shutdown"} {
		t.Run(end, func(t *testing.T) {
			a := &app{s: actionWorkflow(t).s}
			session, csrf := sseCookies(t, a)
			base, stop := context.WithCancel(t.Context())
			defer stop()
			srv := httptest.NewUnstartedServer(a)
			srv.Config.BaseContext = func(net.Listener) context.Context { return base }
			srv.Config.WriteTimeout = 30 * time.Second
			srv.Start()
			defer srv.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.AddCookie(session)
			r.AddCookie(csrf)
			response, err := srv.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatal(response.Status)
			}
			reader := bufio.NewReader(response.Body)
			readFrame := func() string {
				t.Helper()
				ch := make(chan string, 1)
				go func() {
					var frame strings.Builder
					for {
						line, err := reader.ReadString('\n')
						if err != nil {
							ch <- "ERROR: " + err.Error()
							return
						}
						frame.WriteString(line)
						if line == "\n" {
							ch <- frame.String()
							return
						}
					}
				}()
				select {
				case frame := <-ch:
					return frame
				case <-time.After(4 * time.Second):
					cancel()
					t.Fatal("frame not flushed")
					return ""
				}
			}
			initial := readFrame()
			if !strings.Contains(initial, `id="board-content"`) || strings.Contains(initial, `id="notice"`) {
				t.Fatal(initial)
			}
			if _, err := a.s.CreateProjectDetails("Live change", "github", "https://github.com/example/live", "main"); err != nil {
				t.Fatal(err)
			}
			changed := readFrame()
			if !strings.Contains(changed, "Live change") || strings.Contains(changed, `id="notice"`) {
				t.Fatal(changed)
			}
			switch end {
			case "cancel":
				cancel()
			case "revoke":
				if _, err := a.s.db.Exec("DELETE FROM sessions"); err != nil {
					t.Fatal(err)
				}
			case "expire":
				if _, err := a.s.db.Exec("UPDATE sessions SET expires_at=0"); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				stop()
				shutdown, done := context.WithTimeout(t.Context(), 2*time.Second)
				defer done()
				if err := srv.Config.Shutdown(shutdown); err != nil {
					t.Fatal(err)
				}
			}
			finished := make(chan []byte, 1)
			go func() { rest, _ := io.ReadAll(reader); finished <- rest }()
			select {
			case rest := <-finished:
				if len(rest) != 0 {
					t.Fatalf("leaked stream output: %s", rest)
				}
			case <-time.After(3 * time.Second):
				cancel()
				t.Fatal("stream did not terminate")
			}
		})
	}
}

type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (w *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func TestSSEFramesDeadlinesAndHeartbeat(t *testing.T) {
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	stream := newSSEWriter(w)
	if err := stream.write(elementsFrame("<div id=\"notice\">a\nb</div>")); err != nil {
		t.Fatal(err)
	}
	if err := stream.write(": heartbeat\n\n"); err != nil {
		t.Fatal(err)
	}
	if len(w.deadlines) != 2 || w.deadlines[0].Before(time.Now().Add(29*time.Second)) || w.deadlines[1].Before(w.deadlines[0]) {
		t.Fatalf("write deadlines: %v", w.deadlines)
	}
	want := "event: datastar-patch-elements\ndata: elements <div id=\"notice\">a\ndata: elements b</div>\n\n: heartbeat\n\n"
	if w.Body.String() != want {
		t.Fatalf("wire format: %q", w.Body.String())
	}
}

func TestSSEHeartbeatWithoutChanges(t *testing.T) {
	a := &app{s: actionWorkflow(t).s}
	session, csrf := sseCookies(t, a)
	srv := httptest.NewServer(a)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.AddCookie(session)
	r.AddCookie(csrf)
	response, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	reader := bufio.NewReader(response.Body)
	// Consume exactly the initial snapshot. An unchanged workspace must not
	// emit another patch; the next frame is the 15-second keepalive comment.
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\n" {
			break
		}
	}
	started := time.Now()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != ": heartbeat\n" {
		t.Fatalf("unexpected periodic patch: %q", line)
	}
	if time.Since(started) < 14*time.Second {
		t.Fatal("heartbeat sent too early")
	}
	line, err = reader.ReadString('\n')
	if err != nil || line != "\n" {
		t.Fatalf("heartbeat terminator: %q %v", line, err)
	}
}
