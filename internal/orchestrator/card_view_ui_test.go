package orchestrator

import (
	"bytes"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestCardViewShortcut(t *testing.T) {
	for _, state := range []string{"Todo", "Planning", "Building", "Fix", "Done"} {
		for _, archived := range []bool{false, true} {
			c := Card{ID: "card123", Title: `Look <here> & "there"`, Status: state, Archived: archived}
			t.Run(state+"/"+archiveLabel(c), func(t *testing.T) {
				var out bytes.Buffer
				if err := taskCard(pageData{}, c).Render(t.Context(), &out); err != nil {
					t.Fatal(err)
				}
				root, err := html.Parse(strings.NewReader(out.String()))
				if err != nil {
					t.Fatal(err)
				}
				var shortcut, archiveForm, menu *html.Node
				links := 0
				var walk func(*html.Node)
				walk = func(n *html.Node) {
					attrs := map[string]string{}
					for _, a := range n.Attr {
						attrs[a.Key] = a.Val
					}
					if n.Data == "a" && attrs["href"] == planningPath(c.ID) {
						links++
					}
					if attrs["class"] == "icon-btn card-view-action" {
						if shortcut != nil {
							t.Fatal("duplicate shortcut")
						}
						shortcut = n
						if n.Data != "a" || attrs["href"] != planningPath(c.ID) || attrs["aria-label"] != "View card and plan" || attrs["title"] != "View card and plan" {
							t.Fatalf("invalid shortcut: %v", attrs)
						}
						if len(n.Attr) != 4 {
							t.Fatalf("unexpected shortcut behavior: %v", attrs)
						}
						svg := firstElement(n)
						if svg == nil || svg.Data != "svg" || firstElement(svg) == nil || firstElement(svg).Data != "path" || nextElement(firstElement(svg)) == nil || nextElement(firstElement(svg)).Data != "circle" {
							t.Fatal("missing eye SVG")
						}
					}
					if n.Data == "form" && attrs["action"] == archiveRoute(c) {
						archiveForm = n
						if attrs["method"] != "post" {
							t.Fatal("archive method changed")
						}
					}
					if n.Data == "jawa-menu" {
						menu = n
					}
					for child := n.FirstChild; child != nil; child = child.NextSibling {
						walk(child)
					}
				}
				walk(root)
				if links != 3 || shortcut == nil {
					t.Fatalf("expected key, shortcut and menu links, got %d", links)
				}
				if archiveForm == nil || shortcut.Parent != archiveForm.Parent || nextElement(archiveForm) != shortcut || nextElement(shortcut) != menu {
					t.Fatal("shortcut must be between archive form and menu, outside form")
				}
				if !strings.Contains(out.String(), `Look &lt;here&gt; &amp; &#34;there&#34;`) || !strings.Contains(out.String(), " "+archiveLabel(c)+"</button>") {
					t.Fatalf("title escaping or archive label changed: %s", out.String())
				}
			})
		}
	}
}

func TestCardViewShortcutHTTP(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	p, err := a.s.CreateProjectDetails("View fixtures", "github", "https://github.com/example/view", "main")
	if err != nil {
		t.Fatal(err)
	}
	description := strings.Repeat("Full original description. ", 30) + "End of description."
	c, err := a.s.CreateCard(p.ID, "View without planning", description)
	if err != nil {
		t.Fatal(err)
	}
	for _, archived := range []bool{false, true} {
		if err := a.s.SetCardArchived(c.ID, archived); err != nil {
			t.Fatal(err)
		}
		path := boardPath(p.ID)
		if archived {
			path = "/archive?project=" + p.ID
		}
		w := request(a, "GET", path, nil, session, csrf)
		status(t, w, http.StatusOK)
		if !strings.Contains(w.Body.String(), `class="icon-btn card-view-action" href="`+planningPath(c.ID)+`"`) {
			t.Fatal("missing canonical shortcut")
		}
		detail := request(a, "GET", planningPath(c.ID), nil, session, csrf)
		status(t, detail, http.StatusOK)
		if !strings.Contains(detail.Body.String(), "<h1>"+c.Title+"</h1>") || !strings.Contains(detail.Body.String(), description) {
			t.Fatal("missing full card content")
		}
		cards, err := a.s.Cards(p.ID)
		if err != nil || len(cards) != 1 || cards[0].Status != "Todo" || cards[0].Archived != archived {
			t.Fatalf("view mutated card: %v %v", cards, err)
		}
		if _, err := a.s.PlanningDetail(c.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("view started planning: %v", err)
		}
		attempts, err := a.workflow.Attempts()
		if err != nil || len(attempts) != 0 {
			t.Fatalf("view started work: %v %v", attempts, err)
		}
	}
}

func firstElement(n *html.Node) *html.Node {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode {
			return child
		}
	}
	return nil
}
func nextElement(n *html.Node) *html.Node {
	for sibling := n.NextSibling; sibling != nil; sibling = sibling.NextSibling {
		if sibling.Type == html.ElementNode {
			return sibling
		}
	}
	return nil
}
