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
