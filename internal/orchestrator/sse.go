package orchestrator

import (
	"bytes"
	"errors"
	"html"
	"io"
	"net/http"
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
	_ = newSSEWriter(w).write(elementsFrame(notice))
}

// Unsolicited snapshots never include the third (empty notice) fragment. The
// browser's data-ignore-morph on board-content protects a drag in progress;
// activity-panel remains independently morphable.
func (a *app) liveSnapshot(r *http.Request) (string, error) {
	fragments, err := renderFragments(r.Context(), a.s, a.workflow, CSRFToken(r))
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	for _, fragment := range fragments[:2] {
		if err := fragment.Render(r.Context(), &out); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

func (a *app) snapshot(w http.ResponseWriter, r *http.Request, mutation bool) {
	var content string
	var err error
	if mutation {
		content, err = Snapshot(r, a.s, a.workflow)
	} else {
		content, err = a.liveSnapshot(r)
	}
	if err != nil {
		a.responseError(w, r, "Unable to refresh workspace. Please try again.", http.StatusServiceUnavailable)
		return
	}
	stream := newSSEWriter(w)
	if err := stream.write(elementsFrame(content)); err != nil {
		return
	}
	if mutation {
		var signals string
		switch r.URL.Path {
		case "/projects":
			signals = `{"projectOpen":false}`
		case "/cards":
			signals = `{"cardOpen":false}`
		case "/cards/delete":
			signals = `{"deleteOpen":false,"deleteCard":""}`
		}
		if signals != "" {
			_ = stream.write("event: datastar-patch-signals\ndata: signals " + signals + "\n\n")
		}
	}
}

func (a *app) events(w http.ResponseWriter, r *http.Request) {
	content, err := a.liveSnapshot(r)
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
			next, err := a.liveSnapshot(r)
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
