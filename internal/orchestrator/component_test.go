package orchestrator

import (
	"bytes"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestComponentSourceConformance(t *testing.T) {
	files, err := filepath.Glob("*.templ")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := regexp.MustCompile(`<(?:button|select|option|textarea|label)\b`)
	rawInput := regexp.MustCompile(`<input\b[^>]*>`)
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if forbidden.Match(source) {
			t.Errorf("%s contains a raw visible control", file)
		}
		text := string(source)
		// Hidden move-form controls are the only typeless native transport inputs.
		for _, field := range rawInput.FindAllString(text, -1) {
			if strings.Contains(field, `type="hidden"`) {
				continue
			}
			if file == "layout.templ" {
				start := strings.Index(text, `<form id="move-form"`)
				if start >= 0 {
					end := strings.Index(text[start:], "</form>")
					if end >= 0 && strings.Contains(text[start:start+end], field) {
						continue
					}
				}
			}
			t.Errorf("%s has non-transport input: %s", file, field)
		}
	}
	source, err := os.ReadFile("issue_submission_http.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "<input") || forbidden.Match(source) {
		t.Fatal("recovery controls assembled in Go")
	}
}

func TestComponentNativeContracts(t *testing.T) {
	for _, on := range []bool{false, true} {
		var out bytes.Buffer
		if err := switchField("test-switch", "debug", "Debug", "hint", on).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		for _, want := range []string{`data-slot="switch"`, `role="switch"`, `id="test-switch"`, `name="debug" value="true"`, `type="hidden" name="debug" value="false"`} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %s", want)
			}
		}
		if strings.Index(text, `value="true"`) > strings.Index(text, `value="false"`) {
			t.Fatal("switch fallback precedes true")
		}
		if strings.Contains(text, " checked") != on {
			t.Fatal("wrong native switch default")
		}
	}
	for _, state := range []string{"", "created", "completed", "uncertain", "rejected"} {
		r := httptest.NewRequest("POST", "/cards?title=%3Cscript%3E&description=%3C%2Ftextarea%3E&issue_submission=original&create_issue=yes", nil)
		var out bytes.Buffer
		if err := issueRecoveryPage(r, state, "<error>").Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		for _, want := range []string{`data-slot="card"`, `data-slot="alert"`, `data-slot="input"`, `data-slot="textarea"`, `data-slot="button"`, `value="original"`, `&lt;script&gt;`, `&lt;/textarea&gt;`, `&lt;error&gt;`} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %s", state, want)
			}
		}
		recovery := strings.Contains(text, `name="issue_recovery"`)
		if recovery != (state == "uncertain" || state == "rejected") {
			t.Fatalf("wrong recovery eligibility for %s", state)
		}
		if strings.Contains(text, `value="retry"`) != (state == "rejected") {
			t.Fatal("wrong retry eligibility")
		}
	}
}

// Export real Settings and destructive controls for browser validation.
func TestComponentBrowserFixture(t *testing.T) {
	path := os.Getenv("JAWA_COMPONENT_FIXTURE")
	if path == "" {
		t.Skip("fixture export not requested")
	}
	a := testApp(t)
	session, csrf := setupTest(t, a)
	page := request(a, "GET", "/settings", nil, session, csrf)
	if err := os.WriteFile(path, page.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPlanningComponentFixture(t *testing.T) {
	p := Project{ID: "fixture", Name: "Planning fixture", Repo: "example/fixture", BaseBranch: "master"}
	c := Card{ID: "plan", ProjectID: p.ID, Title: "Review reusable controls", Description: "Original request", Status: "Planning"}
	detail := PlanningDetail{Session: PlanningSession{State: "ready", Spec: "## Plan\nKeep the compact Jawa identity.", Revision: 1}, CanApprove: true, Revisions: []PlanningRevision{{Number: 1, Spec: "Reviewed specification"}}}
	d := pageData{v: viewParams{View: viewPlanning, Card: c.ID}, projects: []Project{p}, cards: []Card{c}, planning: map[string]PlanningDetail{c.ID: detail}, now: time.Now()}
	var out bytes.Buffer
	if err := appPage(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, slot := range []string{"card", "checkbox", "native-select", "native-select-option", "badge", "button"} {
		if !strings.Contains(out.String(), `data-slot="`+slot+`"`) {
			t.Errorf("missing %s", slot)
		}
	}
	if path := os.Getenv("JAWA_COMPONENT_FIXTURE"); path != "" {
		if err := os.WriteFile(path+".planning", out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestComponentSwitchPersistence(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	for _, value := range []string{"true", "false", "true"} {
		form := url.Values{"_csrf": {csrf.Value}, "debug": {value}}
		if value == "true" {
			form["debug"] = []string{"true", "false"}
		}
		status(t, request(a, "POST", "/settings/orchestrator", form, session, csrf), 303)
		page := request(a, "GET", "/settings", nil, session, csrf).Body.String()
		patch, err := a.liveSnapshot(httptest.NewRequest("GET", "/events", nil), viewParams{View: viewSettings})
		if err != nil {
			t.Fatal(err)
		}
		for _, markup := range []string{page, patch} {
			pos := strings.Index(markup, `id="runtime-debug"`)
			if pos < 0 {
				t.Fatal("missing native switch")
			}
			native := markup[pos : pos+strings.Index(markup[pos:], ">")]
			if strings.Contains(native, " checked") != (value == "true") {
				t.Fatalf("switch persistence: %s", native)
			}
		}
	}
}
