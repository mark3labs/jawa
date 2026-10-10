package orchestrator

import (
	"bytes"
	"errors"
	"html"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

func datastarRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Datastar-Request"), "true")
}

// Each write gets a fresh deadline: the server's normal WriteTimeout must not
// end healthy long-lived streams, but slow/disconnected readers remain bounded.
type sseWriter struct {
	w          http.ResponseWriter
	controller *http.ResponseController
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	return &sseWriter{w, http.NewResponseController(w)}
}
func (s *sseWriter) write(frame string) error {
	if err := s.controller.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := io.WriteString(s.w, frame); err != nil {
		return err
	}
	return s.controller.Flush()
}

// Datastar joins repeated elements data lines with newlines. Prefix every line
// (including user-generated newlines) so content cannot inject SSE fields.
func elementsFrame(elements string) string {
	elements = strings.ReplaceAll(strings.ReplaceAll(elements, "\r\n", "\n"), "\r", "\n")
	return "event: datastar-patch-elements\ndata: elements " + strings.ReplaceAll(elements, "\n", "\ndata: elements ") + "\n\n"
}

func (a *app) responseError(w http.ResponseWriter, r *http.Request, message string, status int) {
	if !datastarRequest(r) {
		http.Error(w, message, status)
		return
	}
	// Expected action failures are finite 200 responses, not retryable mutations.
	// Escape explicitly: neither markup nor SSE fields may come from an error.
	notice := `<div id="notice" class="notice" role="alert" aria-live="polite">` + html.EscapeString(message) + `</div>`
	stream := newSSEWriter(w)
	if err := stream.write(elementsFrame(notice)); err != nil {
		return
	}
	if r.URL.Path == "/cards/cancel" {
		_ = stream.write("event: datastar-patch-signals\ndata: signals {\"cancelOpen\":false,\"cancelCard\":\"\"}\n\n")
	}
}

// Unsolicited snapshots never include the last (notice) fragment, so a failed
// action stays visible. The browser's data-ignore-morph on the board protects a
// drag in progress; the other roots are independently morphable.
func (a *app) liveSnapshot(r *http.Request, v viewParams) (string, error) {
	fragments, err := renderFragments(r.Context(), a.s, a.workflow, CSRFToken(r), v)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	for _, fragment := range fragments[:len(fragments)-1] {
		if err := fragment.Render(r.Context(), &out); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

// mutationEffects holds what a successful mutation adds to its response.
type mutationEffects struct {
	flash string // info notice shown instead of clearing it
	goTo  string // same-origin path the browser should navigate to
}

func (a *app) snapshot(w http.ResponseWriter, r *http.Request, mutation bool, fx mutationEffects) {
	v := parseViewParams(r, "")
	var content string
	var err error
	if mutation {
		content, err = a.mutationSnapshot(r, v, fx)
	} else {
		content, err = a.liveSnapshot(r, v)
	}
	if err != nil {
		a.responseError(w, r, "Unable to refresh workspace. Please try again.", http.StatusServiceUnavailable)
		return
	}
	stream := newSSEWriter(w)
	if err := stream.write(elementsFrame(content)); err != nil {
		return
	}
	if !mutation {
		return
	}
	signals := map[string]string{}
	switch r.URL.Path {
	case "/projects":
		signals["projectOpen"] = "false"
	case "/cards":
		signals["cardOpen"] = "false"
	case "/cards/cancel":
		signals["cancelOpen"] = "false"
		signals["cancelCard"] = `""`
	case "/cards/delete":
		signals["deleteOpen"] = "false"
		signals["deleteCard"] = `""`
	}
	if fx.goTo != "" {
		signals["goto"] = strconv.Quote(fx.goTo)
	}
	if len(signals) == 0 {
		return
	}
	keys := make([]string, 0, len(signals))
	for k := range signals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, `"`+k+`":`+signals[k])
	}
	_ = stream.write("event: datastar-patch-signals\ndata: signals {" + strings.Join(parts, ",") + "}\n\n")
}

func (a *app) mutationSnapshot(r *http.Request, v viewParams, fx mutationEffects) (string, error) {
	fragments, err := renderFragments(r.Context(), a.s, a.workflow, CSRFToken(r), v)
	if err != nil {
		return "", err
	}
	if fx.flash != "" {
		fragments[len(fragments)-1] = notification(fx.flash, "info")
	}
	var out bytes.Buffer
	for _, fragment := range fragments {
		if err := fragment.Render(r.Context(), &out); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

func (a *app) events(w http.ResponseWriter, r *http.Request) {
	v := parseViewParams(r, "")
	content, err := a.liveSnapshot(r, v)
	if err != nil {
		http.Error(w, "workspace unavailable", http.StatusServiceUnavailable)
		return
	}
	stream := newSSEWriter(w)
	if err := stream.write(elementsFrame(content)); err != nil {
		return
	}
	poll := time.NewTicker(time.Second)
	heartbeat := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
			if r.Context().Err() != nil || !a.authed(r) {
				return
			}
			next, err := a.liveSnapshot(r, v)
			if err != nil {
				return
			} // Never send internal errors into an open stream.
			if next != content {
				if err := stream.write(elementsFrame(next)); err != nil {
					return
				}
				content = next
			}
		case <-heartbeat.C:
			if r.Context().Err() != nil || !a.authed(r) {
				return
			}
			if err := stream.write(": heartbeat\n\n"); err != nil {
				return
			}
		}
	}
}
