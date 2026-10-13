package orchestrator

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCardViewShortcutHTTP(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, csrf := setupTest(t, a)
	p, err := a.s.CreateProjectDetails("View shortcut", "github", "https://github.com/example/view.git", "master")
	if err != nil {
		t.Fatal(err)
	}
	description := "Full original description " + strings.Repeat("without truncation ", 40)
	c, err := a.s.CreateCard(p.ID, "View this card", description)
	if err != nil {
		t.Fatal(err)
	}
	for _, archived := range []bool{false, true} {
		if err := a.s.SetCardArchived(c.ID, archived); err != nil {
			t.Fatal(err)
		}
		route := "/board?project=" + p.ID
		if archived {
			route = "/archive?project=" + p.ID
		}
		page := request(a, "GET", route, nil, session, csrf)
		status(t, page, http.StatusOK)
		want := `class="icon-btn card-view-action" href="` + planningPath(c.ID) + `"`
		if !strings.Contains(page.Body.String(), want) {
			t.Fatal("missing card shortcut")
		}
		detail := request(a, "GET", planningPath(c.ID), nil, session, csrf)
		status(t, detail, http.StatusOK)
		for _, want := range []string{c.Title, strings.TrimSpace(description)} {
			if !strings.Contains(detail.Body.String(), want) {
				t.Errorf("detail missing %q", want)
			}
		}
		if _, err := a.s.PlanningDetail(c.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("view created planning session: %v", err)
		}
		cards, err := a.s.Cards(p.ID)
		if err != nil || len(cards) != 1 || cards[0].Status != "Todo" || cards[0].Archived != archived {
			t.Fatalf("view mutated card: %+v %v", cards, err)
		}
	}
}

func TestCardResetPendingReservationUI(t *testing.T) {
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	a := actionAttempt(t, w, c, "failed", "failed")
	c.Status = "Building"
	d := pageData{attempts: []Attempt{a}, pendingResumes: map[string]bool{c.ID: true}}
	if d.canReset(c) || d.resetReason(c) != resetReason(c, []Attempt{a}, true) {
		t.Fatal("page ignored pending reservation")
	}
	var out strings.Builder
	if err := cardMenu(d, c).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Resume pending — use Force stop first.") || strings.Contains(out.String(), `action="/cards/reset"`) {
		t.Fatalf("pending reset menu: %s", out.String())
	}
	d.pendingResumes[c.ID] = false
	if !d.canReset(c) || d.resetReason(c) != "" {
		t.Fatal("inactive failure not resettable")
	}
	out.Reset()
	if err := cardMenu(d, c).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `action="/cards/reset"`) {
		t.Fatal("Reset action missing after reservation cleared")
	}
}
