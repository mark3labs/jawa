package orchestrator

import (
	"bytes"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func TestPlanningBadgesUseStateIcons(t *testing.T) {
	for _, tc := range []struct {
		state, label, tone string
		spinning           bool
	}{
		{"submitted", "Investigating", "running", true},
		{"running", "Investigating", "running", true},
		{"queued", "Queued", "queued", false},
		{"questions", "Needs input", "blocked", false},
		{"ready", "Ready", "ready", false},
		{"approved", "Approved", "ready", false},
		{"failed", "Failed", "failed", false},
		{"cancelled", "Cancelled", "queued", false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			c := Card{ID: "abc", Title: "Investigate", Status: "Planning"}
			d := pageData{
				v: viewParams{View: viewPlanning, Card: c.ID}, cards: []Card{c},
				planning: map[string]PlanningDetail{c.ID: {Session: PlanningSession{State: tc.state}}},
			}
			for name, component := range map[string]templ.Component{
				"board": cardChips(d, c), "detail": planningContent(d),
			} {
				t.Run(name, func(t *testing.T) {
					var out bytes.Buffer
					if err := component.Render(t.Context(), &out); err != nil {
						t.Fatal(err)
					}
					html := out.String()
					for _, want := range []string{"tone-ic tone-" + tc.tone, "<span>" + tc.label + "</span>", `aria-hidden="true"`} {
						if !strings.Contains(html, want) {
							t.Errorf("missing %q", want)
						}
					}
					if got := strings.Contains(html, `class="spin"`); got != tc.spinning {
						t.Errorf("spinner = %v, want %v", got, tc.spinning)
					}
				})
			}
		})
	}
}
