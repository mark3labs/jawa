package orchestrator

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// Inspect only the requested card/section, including SSE element patches.
func assertCardPRLink(t *testing.T, markup, rootID, want string) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	attr := func(n *html.Node, key string) string {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
		return ""
	}
	var root *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if attr(n, "id") == rootID {
			root = n
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			find(child)
		}
	}
	find(doc)
	if root == nil {
		t.Fatalf("missing root %q", rootID)
	}
	count := 0
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" && attr(n, "title") == "Open pull request" {
			count++
			if attr(n, "href") != want || attr(n, "target") != "_blank" || attr(n, "rel") != "noopener noreferrer" {
				t.Errorf("unexpected PR link attributes: %+v", n.Attr)
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	expected := 0
	if want != "" {
		expected = 1
	}
	if count != expected {
		t.Fatalf("PR links in %s: got %d, want %d", rootID, count, expected)
	}
}

func TestCardPRURLAndRender(t *testing.T) {
	t.Parallel()
	const assigned = "https://codeberg.org/example/repo/pulls/18"
	const reported = "https://github.com/example/repo/pull/19"
	tests := []struct {
		name, status, pr, repair, want string
		noAttempt                      bool
	}{
		{"assigned", "Fix", "", assigned, assigned, false},
		{"reported wins", "Fix", reported, assigned, reported, false},
		{"missing", "Fix", "", "", "", false},
		{"unsafe assignment", "Fix", "", "javascript:alert(1)", "", false},
		{"credential assignment", "Fix", "", "https://user:pass@example.com/pulls/1", "", false},
		{"relative assignment", "Fix", "", "/pull/1", "", false},
		{"hostless assignment", "Fix", "", "https:///pull/1", "", false},
		{"unsafe reported no fallback", "Fix", "javascript:alert(1)", assigned, "", false},
		{"credential reported no fallback", "Fix", "https://user:pass@example.com/pull/1", assigned, "", false},
		{"building reported", "Building", reported, "", reported, false},
		{"done reported", "Done", reported, "", reported, false},
		{"building assignment", "Building", "", assigned, "", false},
		{"done assignment", "Done", "", assigned, "", false},
		{"no attempt", "Fix", "", assigned, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Card{ID: "pr-card", Status: tt.status}
			a := Attempt{CardID: c.ID, Number: 1, State: "queued", PRURL: tt.pr, RepairPRURL: tt.repair}
			d := pageData{cards: []Card{c}, v: viewParams{Card: c.ID}}
			if !tt.noAttempt {
				if got := cardPRURL(c, a); got != tt.want {
					t.Fatalf("URL = %q, want %q", got, tt.want)
				}
				d.attempts = []Attempt{a}
			}
			var out bytes.Buffer
			if err := cardChips(d, c).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			assertCardPRLink(t, `<div id="chips">`+out.String()+`</div>`, "chips", tt.want)
			if strings.Contains(out.String(), ">Ready<") {
				t.Fatal("queued attempt falsely labeled ready")
			}
			out.Reset()
			if err := planningContent(d).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			assertCardPRLink(t, out.String(), "planning-content", tt.want)
		})
	}
	for _, state := range []string{"queued", "running", "blocked", "failed"} {
		t.Run(state, func(t *testing.T) {
			c := Card{ID: "repair", Status: "Fix"}
			d := pageData{attempts: []Attempt{{CardID: c.ID, Number: 2, State: state, RepairPRURL: assigned}}}
			var out bytes.Buffer
			if err := cardChips(d, c).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			assertCardPRLink(t, `<div id="chips">`+out.String()+`</div>`, "chips", assigned)
			if strings.Contains(out.String(), ">Ready<") {
				t.Fatal("repair falsely labeled ready")
			}
		})
	}
}
