package orchestrator

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// Optionally export the real page renderer for browser visual review, with test
// evidence only (no provider requests, credentials, or production data).
func TestCardVisualFixture(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	p := Project{ID: "abc", Name: "Jawa", Provider: "github", Repo: "example/visual-fixtures", BaseBranch: "master"}
	d := pageData{v: viewParams{View: viewBoard, Project: p.ID}, now: now, projects: []Project{p}, prChecks: map[string]PRCheck{}}
	for i, state := range []string{"conflict", "merged", "error"} {
		id := []string{"a1", "a2", "a3"}[i]
		c := Card{ID: id, ProjectID: p.ID, Title: []string{"Resolve a branch conflict", "Ship the completed change", "Retry the provider status check"}[i], Description: "Visual test fixture — not a live provider result.", Status: "Done", IssueURL: "https://github.com/example/visual-fixtures/issues/52", CreatedAt: now}
		d.cards = append(d.cards, c)
		d.attempts = append(d.attempts, Attempt{ID: id, CardID: id, Number: 1, Ready: true, State: "ready", PRURL: "https://github.com/example/visual-fixtures/pull/1", UpdatedAt: now})
		d.prChecks[id] = PRCheck{State: state, CheckedAt: now}
	}
	d.cards = append(d.cards, Card{ID: "b1", ProjectID: p.ID, Title: "Improve card spacing and controls", Description: "Keep actions quiet and let the task content breathe.", Status: "Building", IssueURL: "https://github.com/example/visual-fixtures/issues/52", CreatedAt: now})

	d.planning = map[string]PlanningDetail{}
	for _, fixture := range []struct {
		id, status, title string
		archived          bool
	}{
		{"todo", "Todo", "A new task", false},
		{"planner", "Planning", "Investigate the reusable cards", false},
		{"fixer", "Fix", "Repair the existing pull request", false},
		{"archived-planner", "Planning", "Archived active planning work", true},
		{"unstarted", "Building", "Work not yet started", false},
		{"archived-ready", "Done", "Archived completed work", true},
		{"archived-todo", "Todo", "Archived new task", true},
	} {
		d.cards = append(d.cards, Card{ID: fixture.id, ProjectID: p.ID, Status: fixture.status, Title: fixture.title, Description: "Deterministic shared-card fixture", IssueURL: "https://github.com/example/visual-fixtures/issues/52", CreatedAt: now, Archived: fixture.archived})
		switch fixture.status {
		case "Planning":
			d.planning[fixture.id] = PlanningDetail{Session: PlanningSession{State: "running", UpdatedAt: now}, Turns: []PlanningTurn{{Number: 1, State: "running", AgentID: "planner-" + strings.Repeat("long-identity-", 20), UpdatedAt: now}}}
		case "Done":
			d.attempts = append(d.attempts, Attempt{CardID: fixture.id, Number: 1, State: "ready", Ready: true, PRURL: "https://github.com/example/visual-fixtures/pull/1", UpdatedAt: now})
		case "Fix":
			d.attempts = append(d.attempts, Attempt{CardID: fixture.id, Number: 1, State: "running", RepairPRURL: "https://github.com/example/visual-fixtures/pull/1", AgentID: "fix-agent", UpdatedAt: now})
		}
	}
	d.attempts = append(d.attempts, Attempt{CardID: "b1", Number: 1, State: "running", AgentID: "builder-agent", UpdatedAt: now})
	// Sidebar-only projects exercise each mode, archived work, and zero activity.
	for _, fixture := range []struct {
		id, name, mode string
		archived       bool
	}{
		{"plan", "Planning-only project with a very long name", "Planning", false},
		{"fix", "Fix-only project", "Fix", false},
		{"archive", "Archived active project", "Building", true},
		{"zero", "No active work", "Todo", false},
	} {
		d.projects = append(d.projects, Project{ID: fixture.id, Name: fixture.name})
		d.cards = append(d.cards, Card{ID: fixture.id, ProjectID: fixture.id, Status: fixture.mode, Archived: fixture.archived})
		switch fixture.mode {
		case "Planning":
			d.planning[fixture.id] = PlanningDetail{Session: PlanningSession{State: "running"}, Turns: []PlanningTurn{{State: "running"}}}
		case "Fix", "Building":
			d.attempts = append(d.attempts, Attempt{CardID: fixture.id, Number: 1, State: "running"})
		}
	}

	var out bytes.Buffer
	if err := appPage(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Merge Conflict", "Merged", "Checks Failed", `archive-action"`} {
		if !strings.Contains(out.String(), label) {
			t.Errorf("missing %q", label)
		}
	}
	if strings.Count(out.String(), `action="/prs/refresh"`) != 1 || !strings.Contains(out.String(), "Refresh ready PRs") {
		t.Fatal("expected one column-level refresh action")
	}
	if strings.Count(out.String(), `class="lane-actions"`) != 5 {
		t.Fatal("expected an action row in every lane to align task lists")
	}
	if strings.Contains(out.String(), "Check PR now") {
		t.Fatal("per-card refresh button remains")
	}
	if path := os.Getenv("JAWA_CARD_VISUAL_HTML"); path != "" {
		if err := os.WriteFile(path, out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		d.v.View = viewArchive
		out.Reset()
		if err := appPage(d).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".archive", out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
