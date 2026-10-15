package orchestrator

import (
	"bytes"
	"context"
	"github.com/a-h/templ"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDialogBodyFresh(t *testing.T) {
	a := testApp(t)
	session, csrf := sseCookies(t, a)
	for _, name := range []string{"card", "project"} {
		seen := map[string]bool{}
		for range 2 {
			body := assertSSE(t, sseRequest(a, "GET", "/dialogs/"+name, nil, session, csrf))
			match := regexp.MustCompile(`name="dialog_operation" value="([a-f0-9]{32})"`).FindStringSubmatch(body)
			if len(match) != 2 || seen[match[1]] {
				t.Fatalf("missing fresh operation in %s", body)
			}
			op := match[1]
			seen[op] = true
			for _, want := range []string{`id="dialog-` + op + `"`, `id="` + name + `-error-` + op + `"`, `data-templ-default-open`, `data: mode inner`, `data: selector #dialog-slot`} {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %s", want)
				}
			}
			if name == "card" && strings.Contains(body, `aria-checked="true" name="acknowledge"`) {
				t.Fatal("acknowledgement starts checked")
			}
		}
	}
}

func TestDialogBodyUnknownName(t *testing.T) {
	a := testApp(t)
	session, csrf := sseCookies(t, a)
	for _, path := range []string{"/dialogs/missing", "/dialogs/card/unknown"} {
		status(t, sseRequest(a, "GET", path, nil, session, csrf), http.StatusNotFound)
	}
	status(t, request(a, "GET", "/dialogs/card", nil, session, csrf), http.StatusNotFound)
	status(t, sseRequest(a, "POST", "/dialogs/card", nil, session, csrf), http.StatusNotFound)
}

func TestDialogBodyIneligibleCard(t *testing.T) {
	a := testApp(t)
	session, csrf := sseCookies(t, a)
	for name := range dialogNames {
		if name == "card" || name == "project" {
			continue
		}
		body := assertSSE(t, sseRequest(a, "GET", "/dialogs/"+name+"?card_id=missing", nil, session, csrf))
		if !strings.Contains(body, `id="notice"`) || strings.Contains(body, `data-templ-default-open`) {
			t.Fatalf("%s opened: %s", name, body)
		}
	}
}

func TestSelectFieldOptionsKey(t *testing.T) {
	first := []selectOption{{Value: "1", Label: "First"}}
	if optionsKey(first) != optionsKey(append([]selectOption(nil), first...)) {
		t.Fatal("unstable key")
	}
	for _, other := range [][]selectOption{{{Value: "2", Label: "First"}}, {{Value: "1", Label: "Second"}}, {{Value: "1", Label: "First", Disabled: true}}} {
		if optionsKey(first) == optionsKey(other) {
			t.Fatal("options did not version key")
		}
	}
	var out bytes.Buffer
	key := optionsKey(first)
	if err := selectField("revision", key, "revision", "", false, false, first, nil).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"revision-" + key, "revision-" + key + "-content", "revision-" + key + "-declaration"} {
		if !strings.Contains(out.String(), `id="`+id+`"`) {
			t.Fatal(id)
		}
	}
}

func TestAssignDialogBody(t *testing.T) {
	a := testApp(t)
	for _, lane := range []string{"Todo", "Planning", "Done"} {
		for _, mode := range []string{"build", "plan", "fix"} {
			c := Card{ID: "card", Status: lane}
			d := pageData{cards: []Card{c}, planning: map[string]PlanningDetail{"card": {CanApprove: true, Session: PlanningSession{Revision: 2, Spec: "Reviewed plan"}}}}
			var out bytes.Buffer
			choices := []AgentChoice{{ID: "disabled", Selectable: false}, {ID: "enabled", Selectable: true}}
			if err := assignDialog(d, newOperationID(), c, mode, choices, nil).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			body := out.String()
			needsAck := mode == "build" && lane == "Planning"
			if strings.Contains(body, `name="expected_revision"`) != needsAck {
				t.Fatal("unexpected plan fields")
			}
			if !strings.Contains(body, strings.ReplaceAll(assignLabel(mode, needsAck), "&", "&amp;")) || !strings.Contains(body, `data-disabled`) || !strings.Contains(body, `aria-checked="false"`) && needsAck {
				t.Fatal("missing gate or unchecked acknowledgement")
			}
		}
	}
	_, err := a.dialogComponent(t.Context(), "assign", newOperationID(), pageData{cards: []Card{{ID: "c", Status: "Building"}}}, url.Values{"card_id": {"c"}, "x": {"build"}})
	if err == nil {
		t.Fatal("ineligible assignment opened")
	}
}

// Browser tests use production components and finite SSE, never copied controls.
func TestDatastarBrowserFixture(t *testing.T) {
	dir := os.Getenv("JAWA_DATASTAR_FIXTURE")
	if dir == "" {
		t.Skip("fixture export not requested")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p := Project{ID: "project", Name: "Fixture", Provider: "github", Repo: "example/fixture", BaseBranch: "main"}
	d := pageData{csrf: "fixture-csrf", v: viewParams{View: viewBoard, Project: p.ID}, projects: []Project{p}, now: now, planning: map[string]PlanningDetail{}, prChecks: map[string]PRCheck{}, dashboard: map[string]string{"listen": "127.0.0.1:8080", "nats-listen": "127.0.0.1:4222"}, defaults: map[string]string{}}
	for _, c := range []Card{{ID: "todo", Status: "Todo", Title: "New task"}, {ID: "planning", Status: "Planning", Title: "Review plan"}, {ID: "ready", Status: "Done", Title: "Ready PR"}, {ID: "failed", Status: "Building", Title: "Failed run"}} {
		c.ProjectID = p.ID
		c.CreatedAt = now.Add(-5 * time.Minute)
		d.cards = append(d.cards, c)
	}
	d.attempts = []Attempt{{ID: "ready-attempt", CardID: "ready", Number: 1, State: "ready", Ready: true, PRURL: "https://github.com/example/fixture/pull/7", UpdatedAt: now.Add(-5 * time.Minute)}, {ID: "failed-attempt", CardID: "failed", Number: 1, State: "failed", UpdatedAt: now}}
	d.prChecks["ready"] = PRCheck{State: "clear", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}
	d.planning["planning"] = PlanningDetail{CanApprove: true, Session: PlanningSession{State: "ready", Revision: 2, Spec: "Reviewed specification"}, Revisions: []PlanningRevision{{Number: 1, Spec: "Earlier"}, {Number: 2, Spec: "Reviewed specification"}}}
	write := func(name string, c interface {
		Render(context.Context, io.Writer) error
	}) {
		t.Helper()
		var buf bytes.Buffer
		if err := c.Render(t.Context(), &buf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("board.html", appPage(d))
	d.v = viewParams{View: viewSettings}
	d.nats.Username = "worker"
	d.nats.URL = "nats://127.0.0.1:4222"
	write("settings.html", appPage(d))
	d.v = viewParams{View: viewPlanning, Card: "planning"}
	write("planning.html", appPage(d))
	for name := range dialogNames {
		card := d.cards[2]
		if name == "assign" {
			card = d.cards[1]
		}
		op := strings.Repeat("a", 32)
		var component templ.Component
		switch name {
		case "project":
			component = projectDialog(d, op)
		case "card":
			component = cardDialog(d, op)
		case "fix":
			component = fixDialog(d, op, card, "")
		case "guide":
			component = guideDialog(d, op, card, "token")
		case "delete":
			component = deleteDialog(d, op, card, true)
		case "cancel":
			component = cancelDialog(d, op, card)
		case "force":
			component = forceDialog(d, op, card)
		case "ready":
			component = readyDialog(d, op, d.cards[3], "token", "https://github.com/example/fixture/pull/7")
		case "merge":
			component = mergeDialog(d, op, card, "snapshot", "https://github.com/example/fixture/pull/7", "main", strings.Repeat("a", 40))
		case "assign":
			component = assignDialog(d, op, card, "build", []AgentChoice{{ID: "busy-agent", Provider: "fixture", Model: "offline", Availability: "Busy / not ready", Selectable: true}, {ID: "disabled-agent", Selectable: false}}, nil)
		}
		write(name+".html", component)
	}
	write("issues.html", cardIssueField([]projectIssue{{Number: 1, Title: "Issue title", Body: "Issue body", URL: "https://github.com/example/fixture/issues/1"}}, "Choose an issue, or leave No linked issue selected.", ""))
	write("agents.html", assignmentAgentField([]AgentChoice{{ID: "busy-agent", Selectable: true}}, nil))
	write("recovery.html", issueRecoveryControls("uncertain", "Check repository before recovering."))
	write("blocked-issues.html", cardIssueField(nil, "Check repository before recovering.", "uncertain"))
}
