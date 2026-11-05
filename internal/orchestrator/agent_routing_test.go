package orchestrator

import (
	"bytes"
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/presence"
)

func TestAgentRoutingStoreValidatesAndKeepsOnlyRestrictions(t *testing.T) {
	s, _ := workflowFixture(t)
	if err := s.SaveAgentRouting(AgentRouting{Agents: map[string]AgentRoles{"bad.id": {}}}); err == nil {
		t.Fatal("accepted an invalid agent ID")
	}
	want := AgentRouting{Agents: map[string]AgentRoles{"planner": {Plan: true, Build: true}, "builder": {Build: true}}}
	if err := s.SaveAgentRouting(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentRouting()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 1 || got.Allows("builder", routePlan) || !got.Allows("builder", routeBuild) || !got.Allows("unknown", routePlan) {
		t.Fatalf("routing %+v", got)
	}
	if !got.Restricted(routePlan) || got.Restricted(routeBuild) {
		t.Fatalf("restricted %+v", got)
	}
	// Reset overrides must not clear operator routing policy.
	if err = s.ClearDashboardOverrides(); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.AgentRouting(); got.Allows("builder", routePlan) {
		t.Fatalf("reset cleared routing %+v", got)
	}
}

func TestPickRoutedAgentPrefersAvailableEligibleAgents(t *testing.T) {
	r := AgentRouting{Agents: map[string]AgentRoles{"planner": {Plan: true}}}
	choices := []AgentChoice{
		{ID: "planner", Selectable: true, Availability: "Available"},
		{ID: "busy", Selectable: true, Availability: "Busy / not ready"},
		{ID: "ready", Selectable: true, Availability: "Available"},
		{ID: "no-input", Availability: "Available"},
	}
	for _, task := range []string{"a", "b", "c", "d"} {
		if got, err := pickRoutedAgent(r, choices, routeBuild, task); err != nil || got != "ready" {
			t.Fatalf("task %s routed to %q %v", task, got, err)
		}
	}
	if got, err := pickRoutedAgent(r, choices[:2], routeBuild, "a"); err != nil || got != "busy" {
		t.Fatalf("busy fallback %q %v", got, err)
	}
	if _, err := pickRoutedAgent(r, choices[:1], routeBuild, "a"); err == nil || !strings.Contains(err.Error(), "building") {
		t.Fatalf("no eligible agent %v", err)
	}
}

func TestRestrictedPlanningRoutesToOnlyPlanningAgent(t *testing.T) {
	s, nc := workflowFixture(t)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	registerChoice(t, w, "planner", true)
	registerChoice(t, w, "builder", true)
	if err = s.SaveAgentRouting(AgentRouting{Agents: map[string]AgentRoles{"builder": {Build: true}}}); err != nil {
		t.Fatal(err)
	}
	sub, err := nc.SubscribeSync("bonnie.tasks.>")
	if err != nil {
		t.Fatal(err)
	}
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
	c := workflowCard(t, s)
	if err = w.PlanWithAgent(t.Context(), c.ID, "builder"); err == nil || !strings.Contains(err.Error(), "planning disabled") {
		t.Fatalf("planning-disabled agent accepted: %v", err)
	}
	if err = w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	msg, err := sub.NextMsg(3 * time.Second)
	if err != nil || msg.Subject != "bonnie.tasks.agent.planner" {
		t.Fatalf("planning route %v %v", msg, err)
	}
	// The route is persisted so publish retries and run identity match it.
	var target string
	if err = s.db.QueryRow(`SELECT t.target_agent_id FROM planning_turns t JOIN planning_sessions p ON p.id=t.session_id WHERE p.card_id=?`, c.ID).Scan(&target); err != nil || target != "planner" {
		t.Fatalf("planning target %q %v", target, err)
	}
}

func TestRestrictedBuildsRouteToEligibleAgent(t *testing.T) {
	s, nc := workflowFixture(t)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	registerChoice(t, w, "planner", true)
	if err = s.SaveAgentRouting(AgentRouting{Agents: map[string]AgentRoles{"planner": {Plan: true}}}); err != nil {
		t.Fatal(err)
	}
	sub, err := nc.SubscribeSync("bonnie.tasks.>")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := nc.SubscribeSync("bonnie.tasks")
	if err != nil {
		t.Fatal(err)
	}
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
	c := workflowCard(t, s)
	if err = w.BuildWithAgent(t.Context(), c.ID, "planner"); err == nil || !strings.Contains(err.Error(), "building disabled") {
		t.Fatalf("building-disabled agent accepted: %v", err)
	}
	if err = w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	// Only a build-disabled agent is connected: the work waits, unpublished.
	deadline := time.Now().Add(3 * time.Second)
	for {
		a, _ := w.CardResult(c.ID)
		if strings.Contains(a.Error, "no connected agent is enabled for building") && !a.Published {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting attempt %+v", a)
		}
		time.Sleep(20 * time.Millisecond)
	}
	registerChoice(t, w, "builder", true)
	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil || msg.Subject != "bonnie.tasks.agent.builder" {
		t.Fatalf("build route %v %v", msg, err)
	}
	if a, _ := w.CardResult(c.ID); a.TargetAgentID != "builder" {
		t.Fatalf("persisted target %+v", a)
	}
	if m, err := shared.NextMsg(200 * time.Millisecond); err == nil {
		t.Fatalf("restricted build used the shared queue: %s", m.Subject)
	}
}

func TestAssignmentOptionsDisableRestrictedRole(t *testing.T) {
	choices := []AgentChoice{{ID: "planner", Selectable: true, BuildingDisabled: true}}
	if opts := agentOptions("fix", choices); !opts[1].Disabled || !strings.Contains(opts[1].Label, "Disabled for building") {
		t.Fatalf("fix options %+v", opts)
	}
	if opts := agentOptions("plan", choices); opts[1].Disabled {
		t.Fatalf("plan options %+v", opts)
	}
}

func TestSettingsRoutingMutation(t *testing.T) {
	s, _ := workflowFixture(t)
	a := &app{s: s}
	if err := s.SaveAgentRouting(AgentRouting{Agents: map[string]AgentRoles{"offline": {Build: true}}}); err != nil {
		t.Fatal(err)
	}
	post := func(form url.Values) error {
		r := httptest.NewRequestWithContext(context.Background(), "POST", "/settings/routing", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		return a.mutateSettingsRouting(nil, r, &mutationContext{})
	}
	// Switch order: "true" from the checked switch precedes the hidden "false".
	form := url.Values{"agent": {"planner", "builder"}, "plan:planner": {"true", "false"}, "build:planner": {"false"}, "plan:builder": {"false"}, "build:builder": {"true", "false"}}
	if err := post(form); err != nil {
		t.Fatal(err)
	}
	got, _ := s.AgentRouting()
	if !got.Allows("planner", routePlan) || got.Allows("planner", routeBuild) || got.Allows("builder", routePlan) || got.Allows("offline", routePlan) {
		t.Fatalf("saved %+v", got)
	}
	form.Set("plan:planner", "maybe")
	if err := post(form); err == nil {
		t.Fatal("accepted an invalid switch value")
	}
}

// Live snapshot polling morphs Settings; unsaved role toggles must survive it.
func TestAgentRoutingFormSurvivesLiveSnapshots(t *testing.T) {
	record := func(state presence.State) presence.Record {
		return presence.Record{Identity: presence.Identity{Agent: "worker", Instance: "one"}, State: state, Labels: map[string]string{"jawa.provider": "fixture", "jawa.model": "offline"}, Endpoints: []presence.Endpoint{{Channel: "nats", Address: "bonnie.tasks", Input: true, Ready: true}}}
	}
	render := func(d pageData) string {
		var buf bytes.Buffer
		if err := agentRoutingPanel(d).Render(t.Context(), &buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	busy, ready := pageData{}, pageData{}
	busy.agents = []presence.Record{record(presence.Starting)}
	ready.agents = []presence.Record{record(presence.Ready)}
	first, second := render(busy), render(ready)
	if !strings.Contains(first, "data-ignore-morph") || !strings.Contains(first, `id="agent-routing-form-`) {
		t.Fatalf("routing form is not protected from live morphs: %s", first)
	}
	if first != second {
		t.Fatal("availability changes re-render the routing form")
	}
	saved := ready
	saved.routing = AgentRouting{Agents: map[string]AgentRoles{"worker": {Build: true}}}
	if routingFormKey(saved.routingRows()) == routingFormKey(ready.routingRows()) {
		t.Fatal("saved role changes keep the stale form")
	}
}
