package orchestrator

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPlanningViewParams(t *testing.T) {
	v := parseViewParams(httptest.NewRequest("GET", "/planning?card=abc&project=def", nil), viewPlanning)
	if v.Path() != "/planning?card=abc" {
		t.Fatalf("path: %s", v.Path())
	}
	if !strings.Contains(v.EventsURL(), "view=planning") || !strings.Contains(v.EventsURL(), "card=abc") {
		t.Fatal(v.EventsURL())
	}
	if parsed := parseViewParams(httptest.NewRequest("GET", v.EventsURL(), nil), ""); parsed != v {
		t.Fatalf("lost view: %#v", parsed)
	}
	if v, ok := viewFromPath("/planning"); !ok || v != viewPlanning {
		t.Fatal("planning route missing")
	}
}

func TestPlanningContentEscapesAndFreezesApprovalSelection(t *testing.T) {
	c := Card{ID: "abc", Title: "A card", Description: "<script>original</script>", Status: "Planning"}
	d := pageData{csrf: "csrf", v: viewParams{View: viewPlanning, Card: c.ID},
		cards: []Card{c}, planning: map[string]PlanningDetail{c.ID: {
			Session: PlanningSession{State: "ready", Revision: 2, Spec: "<img src=x onerror=alert(1)>"}, CanApprove: true,
			Revisions: []PlanningRevision{{Number: 1, Spec: "first"}, {Number: 2, Spec: "second"}},
			Messages:  []PlanningMessage{{Role: "assistant", Content: "<script>discussion</script>"}},
		}}}
	var out bytes.Buffer
	if err := planningContent(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"name=\"expected_revision\"", "value=\"1\"", "value=\"2\"", "Select the revision you reviewed", "data-ignore-morph", "Approve &amp; build", "/cards/planning/reply"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"<script>original", "<img src=x", "<script>discussion"} {
		if strings.Contains(html, bad) {
			t.Errorf("unescaped %q", bad)
		}
	}
	detail := d.planning[c.ID]
	detail.CanApprove = false
	detail.Session.State = "questions"
	detail.Questions = []string{"<script>question</script>"}
	d.planning[c.ID] = detail
	out.Reset()
	if err := planningContent(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "id=\"planning-approve-form\"") {
		t.Fatal("approval shown while blocked")
	}
	if !strings.Contains(out.String(), "Needs input") {
		t.Fatal("missing escaped question")
	}
}

func TestPlanningRoutesDurableAndAuthoritative(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	p, err := a.s.CreateProjectDetails("Planning", "github", "https://github.com/example/planning.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.s.CreateCardDetails(p.ID, "Investigate", "Original task", "")
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"_csrf": {csrf.Value}, "view": {"planning"}, "card": {c.ID}, "card_id": {c.ID}}
	for _, route := range []string{"/cards/plan", "/cards/planning/reply", "/cards/planning/retry", "/cards/planning/approve", "/cards/planning/cancel"} {
		status(t, request(a, "GET", route, nil, session, csrf), http.StatusMethodNotAllowed)
		status(t, request(a, "POST", route, nil, session), http.StatusForbidden)
	}
	status(t, request(a, "POST", "/cards/plan", form, session, csrf), http.StatusSeeOther)
	detail, err := a.s.PlanningDetail(c.ID)
	if err != nil || detail.Session.State != "queued" {
		t.Fatalf("durable session: %#v %v", detail, err)
	}
	page := request(a, "GET", planningPath(c.ID), nil, session, csrf)
	status(t, page, http.StatusOK)
	for _, want := range []string{"Original task", "Queued", "planning-content", "view=planning", "card=" + c.ID} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("page missing %q", want)
		}
	}
	form.Set("expected_revision", "garbage")
	status(t, request(a, "POST", "/cards/planning/approve", form, session, csrf), http.StatusBadRequest)
	form.Set("expected_revision", "1")
	status(t, request(a, "POST", "/cards/planning/approve", form, session, csrf), http.StatusBadRequest)
	form.Set("status", "Building")
	status(t, request(a, "POST", "/cards/move", form, session, csrf), http.StatusBadRequest)
	r := httptest.NewRequest("GET", "/snapshot?view=planning&card="+c.ID, nil)
	r.AddCookie(session)
	r.AddCookie(csrf)
	r.Header.Set("Datastar-Request", "true")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	status(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), "planning-content") || strings.Contains(w.Body.String(), "board-content") {
		t.Fatal("incorrect SSE patch root")
	}
	status(t, request(a, "POST", "/cards/planning/cancel", form, session, csrf), http.StatusSeeOther)
	cards, err := a.s.Cards(p.ID)
	if err != nil || len(cards) != 1 || cards[0].Status != "Todo" {
		t.Fatalf("cancelled card: %#v %v", cards, err)
	}
}

func TestPlanningMarkdownSurfacesAndStructuredDiscussion(t *testing.T) {
	raw := `{"spec":"## Human plan\n\nUse **safe** rendering.","questions":["Which \u0060branch\u0060?"],"repo_commit":"` + strings.Repeat("a", 40) + `"}`
	c := Card{ID: "abc", Title: "Card", Description: "# Original", Status: "Planning"}
	detail := PlanningDetail{
		Session:   PlanningSession{State: "questions", Spec: "## Current plan"},
		Revisions: []PlanningRevision{{Number: 1, Spec: "## Old plan", Questions: []string{"**Old question**"}}},
		Questions: []string{"**Current question**"},
		Messages: []PlanningMessage{
			{Role: "assistant", Content: raw},
			{Role: "assistant", Content: "```json\n" + raw + "\n```"},
			{Role: "assistant", Content: "A **natural** reply."},
			{Role: "user", Content: "Use `main`. [bad](javascript:alert%281%29)"},
		},
	}
	d := pageData{v: viewParams{View: viewPlanning, Card: c.ID}, cards: []Card{c}, planning: map[string]PlanningDetail{c.ID: detail}}
	var out bytes.Buffer
	if err := planningContent(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"planning-markdown", "<h1>Original</h1>", "<h2>Current plan</h2>", "<h2>Old plan</h2>", "<strong>Old question</strong>", "<strong>Current question</strong>", "<h2>Human plan</h2>", "<code>branch</code>", "<strong>natural</strong>", "<code>main</code>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	for _, bad := range []string{"repo_commit", strings.Repeat("a", 40), `href="javascript:`} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q", bad)
		}
	}
	if detail.Messages[0].Content != raw {
		t.Fatal("presentation changed original message")
	}
}

func TestPlanningDiscussionFallback(t *testing.T) {
	for _, message := range []PlanningMessage{
		{Role: "assistant", Content: `{"spec":"invalid report"}`},
		{Role: "assistant", Content: "Ordinary **reply**"},
		{Role: "user", Content: `{"spec":"## Not an agent report","repo_commit":"` + strings.Repeat("a", 40) + `"}`},
	} {
		var out bytes.Buffer
		if err := planningDiscussionMessage(message).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), renderPlanningMarkdown(message.Content)) {
			t.Fatal("fallback lost source")
		}
	}
}
