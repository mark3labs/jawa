package orchestrator

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestArchiveRoutesAndViews(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, csrf := setupTest(t, a)
	c := workflowCard(t, a.s)
	form := url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}, "view": {"archive"}}
	status(t, request(a, "GET", "/cards/archive", nil, session, csrf), http.StatusMethodNotAllowed)
	status(t, request(a, "POST", "/cards/archive", nil, session, csrf), http.StatusForbidden)
	status(t, request(a, "POST", "/cards/archive", form), http.StatusSeeOther)
	status(t, request(a, "POST", "/cards/archive", form, session, csrf), http.StatusSeeOther)
	board := request(a, "GET", boardPath(c.ProjectID), nil, session, csrf).Body.String()
	if strings.Contains(board, `id="card-`+c.ID+`"`) {
		t.Fatal("archived card remains on board")
	}
	archive := request(a, "GET", "/archive", nil, session, csrf).Body.String()
	for _, want := range []string{`id="archive-content"`, `id="card-` + c.ID + `"`, `data-bind:filter`, "Restore"} {
		if !strings.Contains(archive, want) {
			t.Errorf("archive missing %q", want)
		}
	}
	body := assertSSE(t, sseRequest(a, "POST", "/cards/restore", form, session, csrf))
	if !strings.Contains(body, `id="archive-content"`) || strings.Contains(body, `id="card-`+c.ID+`"`) {
		t.Fatal("restore did not patch archive")
	}
	board = request(a, "GET", boardPath(c.ProjectID), nil, session, csrf).Body.String()
	if !strings.Contains(board, `id="card-`+c.ID+`"`) {
		t.Fatal("restored card missing from board")
	}
}

func TestMergedReplacesReadyBadge(t *testing.T) {
	t.Parallel()
	c := Card{ID: "abc", Status: "Done"}
	for _, state := range []string{"merged", "clear", "conflict", "error"} {
		t.Run(state, func(t *testing.T) {
			d := pageData{
				attempts: []Attempt{{CardID: c.ID, Number: 1, State: "ready", Ready: true, PRURL: "https://github.com/example/repo/pull/1"}},
				prChecks: map[string]PRCheck{c.ID: {State: state}}}
			var out bytes.Buffer
			if err := taskCard(d, c).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			body := out.String()
			if state == "merged" {
				if strings.Count(body, ">Merged</span>") != 1 || strings.Contains(body, "Ready for review") {
					t.Fatalf("merged should replace ready exactly once: %s", body)
				}
			} else if !strings.Contains(body, "Ready for review") || strings.Contains(body, ">Merged</span>") {
				t.Fatalf("non-merged check replaced historical readiness: %s", body)
			}
		})
	}
}
