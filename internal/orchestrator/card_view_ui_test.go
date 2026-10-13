package orchestrator

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"testing"
)

func TestCardViewShortcut(t *testing.T) {
	for _, state := range []string{"Todo", "Planning", "Building", "Fix", "Done"} {
		for _, archived := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/archived=%t", state, archived), func(t *testing.T) {
				c := Card{ID: "card&special", Title: "View <card> & plan", Status: state, Archived: archived}
				var out bytes.Buffer
				if err := taskCard(pageData{}, c).Render(t.Context(), &out); err != nil {
					t.Fatal(err)
				}
				markup := out.String()
				href := `href="` + html.EscapeString(planningPath(c.ID)) + `"`
				start := strings.Index(markup, `<a class="icon-btn card-view-action"`)
				if start < 0 || strings.Count(markup, `class="icon-btn card-view-action"`) != 1 {
					t.Fatal("expected one shortcut")
				}
				end := strings.Index(markup[start:], "</a>")
				if end < 0 {
					t.Fatal("missing closing anchor")
				}
				link := markup[start : start+end]
				for _, want := range []string{href, `aria-label="View card and plan: ` + html.EscapeString(cardKey(nil, c)) + `"`, `title="View card and plan"`, `aria-hidden="true"`, `<path d=`, `<circle cx="12" cy="12" r="3"`} {
					if !strings.Contains(link, want) {
						t.Errorf("shortcut missing %s", want)
					}
				}
				formEnd := strings.Index(markup, "</form>")
				menuStart := strings.Index(markup, "<jawa-menu>")
				if formEnd < 0 || formEnd > start || menuStart < start {
					t.Fatal("shortcut must follow the archive form and precede the menu")
				}
				for _, want := range []string{`method="post" action="` + archiveRoute(c) + `"`, archiveLabel(c), `class="card-key"`, `View card &amp; plan</a>`, html.EscapeString(c.Title)} {
					if !strings.Contains(markup, want) {
						t.Errorf("card missing %s", want)
					}
				}
				if strings.Count(markup, href) != 3 {
					t.Fatal("card key, shortcut and menu must retain the detail destination")
				}
				if strings.Contains(markup, "<card>") {
					t.Fatal("unescaped title")
				}
			})
		}
	}
}

func TestCardViewShortcutHTTP(t *testing.T) {
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
