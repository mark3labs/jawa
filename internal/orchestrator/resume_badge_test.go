package orchestrator

import (
	"bytes"
	"github.com/a-h/templ"
	"strings"
	"testing"
)

func TestPendingResumeBadges(t *testing.T) {
	a := Attempt{State: "running", RunState: "running", Error: resumePendingNotice}
	if runTone(a) != "resuming" || runLabel(a) != "Resuming…" || !attemptActive(a) {
		t.Fatal("pending resume must be visible and still exclude competing work")
	}
	a.Error = ""
	if runTone(a) != "running" || runLabel(a) != "Running" {
		t.Fatal("ordinary execution changed")
	}
	c := Card{ID: "card", Title: "Investigate", Status: "Planning"}
	d := pageData{v: viewParams{View: viewPlanning, Card: c.ID}, cards: []Card{c}, planning: map[string]PlanningDetail{c.ID: {Session: PlanningSession{State: "running", Error: resumePendingNotice}}}}
	for name, component := range map[string]templ.Component{"board": cardChips(d, c), "detail": planningContent(d)} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := component.Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Resuming…") || !strings.Contains(out.String(), "tone-resuming") {
				t.Fatal(out.String())
			}
		})
	}
}
