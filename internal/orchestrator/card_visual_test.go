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
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	p := Project{ID: "abc", Name: "Card status fixtures", Provider: "github", Repo: "example/visual-fixtures", BaseBranch: "main"}
	d := pageData{v: viewParams{View: viewBoard, Project: p.ID}, now: now, projects: []Project{p}, prChecks: map[string]PRCheck{}}
	for i, state := range []string{"conflict", "merged", "error"} {
		id := []string{"a1", "a2", "a3"}[i]
		c := Card{ID: id, ProjectID: p.ID, Title: []string{"Resolve a branch conflict", "Ship the completed change", "Retry the provider status check"}[i], Description: "Visual test fixture — not a live provider result.", Status: "Done", CreatedAt: now}
		d.cards = append(d.cards, c)
		d.attempts = append(d.attempts, Attempt{ID: id, CardID: id, Number: 1, Ready: true, State: "ready", PRURL: "https://github.com/example/visual-fixtures/pull/1", UpdatedAt: now})
		d.prChecks[id] = PRCheck{State: state, CheckedAt: now}
	}
	d.cards = append(d.cards, Card{ID: "b1", ProjectID: p.ID, Title: "Improve card spacing and controls", Description: "Keep actions quiet and let the task content breathe.", Status: "Building", CreatedAt: now})
	var out bytes.Buffer
	if err := appPage(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Merge Conflict", "Merged", "Checks Failed", `class="archive-action"`} {
		if !strings.Contains(out.String(), label) {
			t.Errorf("missing %q", label)
		}
	}
	if strings.Count(out.String(), `action="/prs/refresh"`) != 1 || !strings.Contains(out.String(), "Refresh ready PRs") {
		t.Fatal("expected one column-level refresh action")
	}
	if strings.Contains(out.String(), "Check PR now") {
		t.Fatal("per-card refresh button remains")
	}
	if path := os.Getenv("JAWA_CARD_VISUAL_HTML"); path != "" {
		if err := os.WriteFile(path, out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		d.v.View = viewArchive
		d.cards[len(d.cards)-1].Archived = true
		out.Reset()
		if err := appPage(d).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".archive", out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
