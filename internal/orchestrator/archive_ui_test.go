package orchestrator

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestArchiveRoutesAndViews(t *testing.T) {
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

func TestCardPRBadges(t *testing.T) {
	for state, label := range map[string]string{"conflict": "Merge Conflict", "merged": "Merged", "error": "Checks Failed"} {
		t.Run(state, func(t *testing.T) {
			c := Card{ID: "abc"}
			d := pageData{prChecks: map[string]PRCheck{c.ID: {State: state}}}
			var out bytes.Buffer
			if err := cardPRStatus(d, c).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), `class="chip `) || !strings.Contains(out.String(), label) {
				t.Fatalf("missing badge: %s", out.String())
			}
		})
	}
}
