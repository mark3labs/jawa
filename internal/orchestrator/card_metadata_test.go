package orchestrator

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
)

func TestCardPlanningAssignmentMatrix(t *testing.T) {
	for _, tc := range []struct {
		session, turn string
		assigned      bool
	}{
		{"queued", "queued", false}, {"submitted", "submitted", true},
		{"running", "running", true}, {"questions", "waiting", true},
		{"ready", "waiting", true}, {"running", "queued", false},
		{"failed", "failed", false}, {"cancelled", "cancelled", false},
		{"approved", "waiting", false}, {"running", "completed", false},
	} {
		t.Run(tc.session+"/"+tc.turn, func(t *testing.T) {
			c := Card{ID: "card", Status: "Planning"}
			agent := "<agent>&" + strings.Repeat("long", 40)
			detail := PlanningDetail{Session: PlanningSession{ID: "session", State: tc.session}, Turns: []PlanningTurn{
				{SessionID: "session", Number: 2, State: tc.turn, AgentID: agent},
				{SessionID: "session", Number: 1, State: "running", AgentID: "stale-planner"},
				{SessionID: "old-session", Number: 99, State: "running", AgentID: "wrong-session"},
			}}
			d := pageData{planning: map[string]PlanningDetail{c.ID: detail}, attempts: []Attempt{{CardID: c.ID, State: "running", AgentID: "old-builder", Error: "old-error"}}}
			var out bytes.Buffer
			if err := taskCard(d, c).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			html := out.String()
			count := 0
			if tc.assigned {
				count = 1
			}
			if strings.Count(html, `title="Assigned agent"`) != count {
				t.Fatalf("assignment count: %s", html)
			}
			for _, stale := range []string{"stale-planner", "wrong-session", "old-builder", "old-error", `title="View runs"`} {
				if strings.Contains(html, stale) {
					t.Errorf("leaked %s", stale)
				}
			}
			if tc.assigned && !strings.Contains(html, "&lt;agent&gt;&amp;") {
				t.Fatal("agent not escaped")
			}
		})
	}
	c := Card{ID: "card", Status: "Planning"}
	for _, detail := range []PlanningDetail{{}, {Session: PlanningSession{State: "running"}}} {
		d := pageData{planning: map[string]PlanningDetail{c.ID: detail}}
		if d.cardAgent(c) != "" {
			t.Fatal("assignment without turn")
		}
	}
	if (pageData{}).cardAgent(c) != "" {
		t.Fatal("assignment without detail")
	}
}

func TestCardSharedAssignmentAndDiagnostics(t *testing.T) {
	c := Card{ID: "card", Status: "Planning"}
	detail := PlanningDetail{Session: PlanningSession{State: "running"}, Turns: []PlanningTurn{{Number: 1, State: "running", AgentID: "worker"}}}
	d := pageData{planning: map[string]PlanningDetail{c.ID: detail}, attempts: []Attempt{{CardID: c.ID, State: "running", AgentID: "worker"}}}
	var planner bytes.Buffer
	if err := cardChips(d, c).Render(t.Context(), &planner); err != nil {
		t.Fatal(err)
	}
	for _, workflow := range []string{"Building", "Fix"} {
		c.Status = workflow
		var builder bytes.Buffer
		if err := cardChips(d, c).Render(t.Context(), &builder); err != nil {
			t.Fatal(err)
		}
		const chip = `<span class="chip plain" title="Assigned agent">`
		if strings.Count(builder.String(), chip) != 1 || strings.Count(planner.String(), chip) != 1 {
			t.Fatal("different assignment markup")
		}
		if d.cardAgent(c) != "worker" {
			t.Fatal("transition did not select builder")
		}
	}
	diagnostic := "<failure>&"
	d.attempts[0].State = "failed"
	d.attempts[0].RunState = "failed"
	d.attempts[0].Error = diagnostic
	detail.Session.State = "failed"
	detail.Session.Error = diagnostic
	d.planning[c.ID] = detail
	var previous string
	for _, workflow := range []string{"Planning", "Building"} {
		c.Status = workflow
		var out bytes.Buffer
		if err := cardNote(d, c).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `class="card-note tone-failed"`) || !strings.Contains(out.String(), "&lt;failure&gt;&amp;") {
			t.Fatal(out.String())
		}
		if previous != "" && previous != out.String() {
			t.Fatal("different diagnostic markup")
		}
		previous = out.String()
	}
	c.Status = "Planning"
	detail.Session.State = "running"
	detail.Session.Error = resumePendingNotice
	d.planning[c.ID] = detail
	if note, _ := d.cardDiagnostic(c); note != "" {
		t.Fatal("resume notice duplicated")
	}
	if planningState(d, c.ID) != "resuming" {
		t.Fatal("lost resuming badge")
	}
	detail.Session.Error = "warning"
	d.planning[c.ID] = detail
	if note, tone := d.cardDiagnostic(c); note != "warning" || tone == "failed" {
		t.Fatal("warning is failure")
	}
}

func TestCardActivityTime(t *testing.T) {
	base := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                                   string
		creation, attempt, session, turn, want time.Duration
	}{
		{"creation", 0, -time.Hour, -time.Hour, -time.Hour, 0},
		{"planning", 0, 0, time.Hour, 0, time.Hour},
		{"execution", 0, 3 * time.Hour, time.Hour, 2 * time.Hour, 3 * time.Hour},
		{"turn", 0, time.Hour, time.Hour, 2 * time.Hour, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Card{ID: "card", CreatedAt: base.Add(tc.creation)}
			d := pageData{now: base.Add(4 * time.Hour), attempts: []Attempt{{CardID: c.ID, UpdatedAt: base.Add(tc.attempt)}}, planning: map[string]PlanningDetail{c.ID: {Session: PlanningSession{UpdatedAt: base.Add(tc.session)}, Turns: []PlanningTurn{{UpdatedAt: base.Add(tc.turn)}}}}}
			want := base.Add(tc.want)
			if !d.cardActivity(c).Equal(want) {
				t.Fatal(d.cardActivity(c))
			}
			var out bytes.Buffer
			if err := cardChips(d, c).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), `datetime="`+timeAttr(want)+`"`) || !strings.Contains(out.String(), relativeTime(want, d.now)) {
				t.Fatal(out.String())
			}
		})
	}
	if !(pageData{}).cardActivity(Card{}).IsZero() {
		t.Fatal("zero timestamp changed")
	}
	c := Card{CreatedAt: base}
	if !(pageData{}).cardActivity(c).Equal(base) {
		t.Fatal("missing detail changed creation")
	}
}

func TestPersistedPlannerAssignmentSnapshot(t *testing.T) {
	s, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	turn := planningDetails(t, w, c.ID).Turns[0]
	event := client.StatusEvent{TaskID: turn.ID, AgentID: "persisted-planner", RunID: "run", AttemptID: "attempt", State: runtime.RunRunning, Seq: 1}
	if err := w.event(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if got := planningDetails(t, w, c.ID).Turns[0].AgentID; got != event.AgentID {
		t.Fatalf("persisted identity = %q", got)
	}
	for _, archived := range []bool{false, true, false} {
		if _, err := s.db.Exec(`UPDATE cards SET archived=? WHERE id=?`, archived, c.ID); err != nil {
			t.Fatal(err)
		}
		snap, err := loadUISnapshot(t.Context(), s, nil)
		if err != nil {
			t.Fatal(err)
		}
		d := pageData{uiSnapshot: snap, v: viewParams{View: viewBoard}}
		if archived {
			d.v.View = viewArchive
		}
		var full, patch bytes.Buffer
		if err := appPage(d).Render(t.Context(), &full); err != nil {
			t.Fatal(err)
		}
		// SSE patches and pages consume the same snapshot/card component.
		for _, card := range snap.cards {
			if card.ID == c.ID {
				if err := taskCard(d, card).Render(t.Context(), &patch); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, html := range []string{full.String(), patch.String()} {
			if !strings.Contains(html, event.AgentID) {
				t.Fatal("snapshot lost assignment")
			}
		}
	}
	if _, err := s.db.Exec(`UPDATE planning_turns SET state='queued' WHERE id=?`, turn.ID); err != nil {
		t.Fatal(err)
	}
	snap, err := loadUISnapshot(t.Context(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := pageData{uiSnapshot: snap}
	if d.cardAgent(Card{ID: c.ID, Status: "Planning"}) != "" {
		t.Fatal("snapshot retained stale assignment")
	}
}

func TestPlannerAssignmentFullPageAndSSEArchiveRestore(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	c := workflowCard(t, a.s)
	w := &Workflow{s: a.s, ctx: t.Context()}
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	turn := planningDetails(t, w, c.ID).Turns[0]
	event := client.StatusEvent{TaskID: turn.ID, AgentID: "snapshot-worker", RunID: "run", AttemptID: "attempt", State: runtime.RunRunning, Seq: 1}
	if err := w.event(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}, "view": {"board"}, "project": {c.ProjectID}}
	assertAssignment := func(body string, want bool) {
		t.Helper()
		if strings.Contains(body, event.AgentID) != want {
			t.Fatalf("assignment presence = %v, want %v", !want, want)
		}
	}
	assertAssignment(request(a, "GET", boardPath(c.ProjectID), nil, session, csrf).Body.String(), true)
	assertAssignment(assertSSE(t, sseRequest(a, "POST", "/cards/archive", form, session, csrf)), false)
	assertAssignment(request(a, "GET", "/archive", nil, session, csrf).Body.String(), true)
	form.Set("view", "archive")
	assertAssignment(assertSSE(t, sseRequest(a, "POST", "/cards/restore", form, session, csrf)), false)
	assertAssignment(request(a, "GET", boardPath(c.ProjectID), nil, session, csrf).Body.String(), true)
	if _, err := a.s.db.Exec(`UPDATE planning_turns SET state='queued' WHERE id=?`, turn.ID); err != nil {
		t.Fatal(err)
	}
	// Restoring an already-visible card is idempotent and patches its current view.
	form.Set("view", "board")
	assertAssignment(assertSSE(t, sseRequest(a, "POST", "/cards/restore", form, session, csrf)), false)
	assertAssignment(request(a, "GET", boardPath(c.ProjectID), nil, session, csrf).Body.String(), false)
}
