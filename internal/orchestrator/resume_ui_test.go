package orchestrator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"golang.org/x/net/html"
)

func TestFailedResumeCardMenu(t *testing.T) {
	t.Parallel()
	raw, _ := json.Marshal(client.Outcome{Version: 1, TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed})
	attempt := Attempt{CardID: "card", TaskID: "task", AgentID: "agent", RunID: "run", RemoteAttemptID: "remote", State: "failed", RunState: "failed", OutcomeJSON: string(raw)}
	for _, tc := range []struct {
		name, lane, route, token string
		detail                   *PlanningDetail
	}{
		{name: "build", lane: "Building", route: "/cards/resume", token: resumeToken(attempt)},
		{name: "fix", lane: "Fix", route: "/cards/resume", token: resumeToken(attempt)},
		{name: "planning with workflow history", lane: "Planning", route: "/cards/planning/resume", token: "planning-token", detail: &PlanningDetail{CanResume: true, ResumeToken: "planning-token"}},
		{name: "missing planning detail", lane: "Planning"},
		{name: "ineligible planning", lane: "Planning", detail: &PlanningDetail{ResumeToken: "planning-token"}},
		{name: "todo", lane: "Todo"},
		{name: "done", lane: "Done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := Card{ID: "card", Title: "Card", Status: tc.lane}
			d := pageData{csrf: "csrf-token", v: viewParams{View: viewBoard, Project: "project"}, attempts: []Attempt{attempt}, planning: map[string]PlanningDetail{}}
			if tc.detail != nil {
				d.planning[card.ID] = *tc.detail
			}
			var buf bytes.Buffer
			if err := cardMenu(d, card).Render(t.Context(), &buf); err != nil {
				t.Fatal(err)
			}
			count := strings.Count(buf.String(), "Resume failed run")
			if tc.route == "" {
				if count != 0 {
					t.Fatal("ineligible resume shown")
				}
				return
			}
			if count != 1 {
				t.Fatalf("resume items = %d", count)
			}
			doc, err := html.Parse(strings.NewReader(buf.String()))
			if err != nil {
				t.Fatal(err)
			}
			var form *html.Node
			var visit func(*html.Node)
			visit = func(n *html.Node) {
				if n.Type == html.ElementNode && n.Data == "form" {
					for _, a := range n.Attr {
						if a.Key == "action" && a.Val == tc.route {
							form = n
						}
					}
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					visit(c)
				}
			}
			visit(doc)
			if form == nil {
				t.Fatal("missing resume form")
			}
			fields := map[string]string{}
			visit = func(n *html.Node) {
				if n.Type == html.ElementNode && n.Data == "input" {
					attrs := map[string]string{}
					for _, a := range n.Attr {
						attrs[a.Key] = a.Val
					}
					if attrs["type"] != "hidden" {
						t.Fatal("non-hidden field")
					}
					fields[attrs["name"]] = attrs["value"]
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					visit(c)
				}
			}
			visit(form)
			for key, want := range map[string]string{"_csrf": "csrf-token", "card_id": "card", "view": "board", "project": "project", "expected_resume": tc.token} {
				if fields[key] != want {
					t.Errorf("%s = %q, want %q", key, fields[key], want)
				}
			}
			attrs := map[string]string{}
			for _, a := range form.Attr {
				attrs[a.Key] = a.Val
			}
			if attrs["method"] != "post" || !strings.Contains(buf.String(), "selector:&#39;#"+attrs["id"]) {
				t.Fatal("submission behavior changed", attrs)
			}
		})
	}
}
