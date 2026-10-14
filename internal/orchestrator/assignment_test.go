package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/presence"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/orchestrator/db/sqlc"
	"jawa/internal/reportprotocol"
)

func registerChoice(t *testing.T, w *Workflow, id string, input bool) {
	t.Helper()
	err := w.presence.Register(t.Context(), presence.Record{Identity: presence.Identity{Agent: id, Instance: "test"}, State: presence.Starting, Labels: map[string]string{"jawa.provider": "fixture", "jawa.model": "offline", "secret": "not-for-browser"}, Endpoints: []presence.Endpoint{{Channel: "nats", Address: "bonnie.tasks.agent." + id, Input: input}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAssignmentRoutesAndAdmission(t *testing.T) {
	s, nc := workflowFixture(t)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	registerChoice(t, w, "chosen", true)
	registerChoice(t, w, "other", true)
	registerChoice(t, w, "no-input", false)
	sub, err := nc.SubscribeSync("bonnie.tasks.>")
	if err != nil {
		t.Fatal(err)
	}
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
	c := workflowCard(t, s)
	for _, id := range []string{"", "unknown", "chosen.*", "no-input"} {
		if err = w.BuildWithAgent(t.Context(), c.ID, id); err == nil {
			t.Fatalf("accepted %q", id)
		}
	}
	assertActionLane(t, w, c.ProjectID, "Todo", c.ID)
	if err = w.BuildWithAgent(t.Context(), c.ID, "chosen"); err != nil {
		t.Fatal(err)
	}
	if err = w.BuildWithAgent(t.Context(), c.ID, "other"); err == nil {
		t.Fatal("duplicate build")
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.TargetAgentID != "chosen" || a.AgentID != "" || a.RunID != "" {
		t.Fatalf("assignment %+v", a)
	}
	msg, err := sub.NextMsg(3 * time.Second)
	if err != nil || msg.Subject != "bonnie.tasks.agent.chosen" {
		t.Fatalf("route %v %v", msg, err)
	}
	var task client.Task
	if err = json.Unmarshal(msg.Data, &task); err != nil || task.TaskID != a.TaskID {
		t.Fatalf("task %v", err)
	}
	wrong := client.Target{TaskID: a.TaskID, AgentID: "other", RunID: "run", AttemptID: "remote"}
	if err = w.event(t.Context(), client.StatusEvent{Version: 1, Target: wrong, State: runtime.RunRunning, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if err = w.result(t.Context(), client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "other", RunID: "run", AttemptID: "remote", State: runtime.RunFailed}); err != nil {
		t.Fatal(err)
	}
	after, _ := w.CardResult(c.ID)
	if after.AgentID != "" || after.OutcomeJSON != "" {
		t.Fatalf("wrong agent admitted %+v", after)
	}
	right := wrong
	right.AgentID = "chosen"
	if err = w.event(t.Context(), client.StatusEvent{Version: 1, Target: right, State: runtime.RunRunning, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if err = w.event(t.Context(), client.StatusEvent{Version: 1, Target: wrong, State: runtime.RunFailed, Seq: 2}); err != nil {
		t.Fatal(err)
	}
	after, _ = w.CardResult(c.ID)
	if after.AgentID != "chosen" || after.RunState == "failed" {
		t.Fatalf("pin %+v", after)
	}
	if err = w.result(t.Context(), client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "chosen", RunID: "run", AttemptID: "remote", State: runtime.RunFailed, Error: "fixture failure"}); err != nil {
		t.Fatal(err)
	}
	if err = w.presence.Unregister(t.Context(), presence.Identity{Agent: "chosen", Instance: "test"}); err != nil {
		t.Fatal(err)
	}
	if err = w.Retry(c.ID); err != nil {
		t.Fatal(err)
	}
	retry, _ := w.CardResult(c.ID)
	if retry.TargetAgentID != "chosen" || retry.TaskID == a.TaskID {
		t.Fatalf("retry %+v", retry)
	}
	msg, err = sub.NextMsg(3 * time.Second)
	if err != nil || msg.Subject != "bonnie.tasks.agent.chosen" {
		t.Fatalf("offline retry %v %v", msg, err)
	}
	sharedSub, err := nc.SubscribeSync("bonnie.tasks")
	if err != nil {
		t.Fatal(err)
	}
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
	shared := workflowCard(t, s)
	if err = w.MoveCard(shared.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	msg, err = sharedSub.NextMsg(3 * time.Second)
	if err != nil || msg.Subject != "bonnie.tasks" {
		t.Fatalf("shared route %v %v", msg, err)
	}
}

func TestTargetedPlanningApprovalAndRepair(t *testing.T) {
	s, nc := workflowFixture(t)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	registerChoice(t, w, "planner", true)
	registerChoice(t, w, "builder", true)
	sub, err := nc.SubscribeSync("bonnie.tasks.>")
	if err != nil {
		t.Fatal(err)
	}
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
	assertRoute := func(subject string) {
		t.Helper()
		msg, err := sub.NextMsg(3 * time.Second)
		if err != nil || msg.Subject != subject {
			t.Fatalf("route %v %v expected %s", msg, err, subject)
		}
	}
	c := workflowCard(t, s)
	if err = w.PlanWithAgent(t.Context(), c.ID, "planner"); err != nil {
		t.Fatal(err)
	}
	assertRoute("bonnie.tasks.agent.planner")
	d := planningDetails(t, w, c.ID)
	if d.Turns[0].TargetAgentID != "planner" || d.Turns[0].AgentID != "" {
		t.Fatalf("turn %+v", d.Turns)
	}
	if err = planningDelivery(t, w, planningOutcome(t, w, c.ID, "Reviewed specification")); err != nil {
		t.Fatal(err)
	}
	if err = w.ApprovePlanningWithAgent(t.Context(), c.ID, 99, "builder"); err == nil {
		t.Fatal("stale approval")
	}
	if err = w.ApprovePlanningWithAgent(t.Context(), c.ID, 1, "builder"); err != nil {
		t.Fatal(err)
	}
	assertRoute("bonnie.tasks.agent.builder")
	a, _ := w.CardResult(c.ID)
	if a.TargetAgentID != "builder" || !strings.Contains(a.TaskJSON, "Reviewed specification") {
		t.Fatalf("approved %+v", a)
	}
	// Ready evidence is fixture-only; repairs preserve the server-owned PR/branch.
	if _, err = s.db.Exec(`UPDATE workflow_attempts SET state='blocked',run_state='completed',outcome_json='{}',pr_url='https://github.com/example/repo/pull/18' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	a, _ = w.CardResult(c.ID)
	if err = w.markReady(a); err != nil {
		t.Fatal(err)
	}
	if err = w.FixWithAgent(t.Context(), c.ID, " ", "planner"); err == nil {
		t.Fatal("blank repair")
	}
	if err = w.FixWithAgent(t.Context(), c.ID, "Fix review feedback", "planner"); err != nil {
		t.Fatal(err)
	}
	assertRoute("bonnie.tasks.agent.planner")
	repair, _ := w.CardResult(c.ID)
	branch, _ := assignedBranch(a)
	if repair.TargetAgentID != "planner" || repair.RepairPRURL != a.PRURL || repair.Branch != branch {
		t.Fatalf("repair %+v", repair)
	}
}

func TestTargetedCancelledPlanningAndRetry(t *testing.T) {
	s, w, c := planningLocal(t)
	if err := w.startPlanningTo(c.ID, 0, "planner"); err != nil {
		t.Fatal(err)
	}
	d := planningDetails(t, w, c.ID)
	task := d.Turns[0].ID
	if err := w.CancelPlanning(c.ID); err != nil {
		t.Fatal(err)
	}
	ev := client.StatusEvent{Version: 1, Target: client.Target{TaskID: task, AgentID: "other", RunID: "run", AttemptID: "remote"}, Seq: 1, State: runtime.RunRunning}
	if err := w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	turn, err := sqlc.New(s.db).GetPlanningTurn(t.Context(), task)
	if err != nil || turn.AgentID != "" {
		t.Fatalf("cancelled late admission %+v %v", turn, err)
	}
	ev.AgentID = "planner"
	if err = w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	turn, _ = sqlc.New(s.db).GetPlanningTurn(t.Context(), task)
	if turn.AgentID != "planner" || turn.State != "cancelled" {
		t.Fatalf("cancelled %+v", turn)
	}
	c = workflowCard(t, s)
	if err = w.startPlanningTo(c.ID, 0, "planner"); err != nil {
		t.Fatal(err)
	}
	d = planningDetails(t, w, c.ID)
	if err = w.result(t.Context(), client.Outcome{Version: 1, TaskID: d.Turns[0].ID, AgentID: "planner", RunID: "run", AttemptID: "remote", State: runtime.RunFailed, Error: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err = w.RetryPlanning(c.ID); err != nil {
		t.Fatal(err)
	}
	d = planningDetails(t, w, c.ID)
	if d.Turns[1].TargetAgentID != "planner" {
		t.Fatalf("retry %+v", d.Turns)
	}
}

func TestAgentChoicesProjectionAndHTTP(t *testing.T) {
	records := []presence.Record{{Identity: presence.Identity{Agent: "busy", Instance: "one"}, Labels: map[string]string{"jawa.provider": "fixture", "secret": "hidden"}, Endpoints: []presence.Endpoint{{Channel: "nats", Address: "bonnie.tasks", Input: true}}}, {Identity: presence.Identity{Agent: "busy", Instance: "two"}}, {Identity: presence.Identity{Agent: "a"}}}
	choices := agentChoices(records)
	if len(choices) != 2 || choices[0].ID != "a" || !choices[1].Selectable || choices[1].Availability != "Busy / not ready" {
		t.Fatalf("choices %+v", choices)
	}
	if !reflect.DeepEqual(choices, agentChoices(records)) {
		t.Fatal("unstable choices")
	}
	s, nc := workflowFixture(t)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	registerChoice(t, w, "busy", true)
	a := &app{s: s, workflow: w}
	r := httptest.NewRequest(http.MethodGet, "/agents/choices", nil)
	response := httptest.NewRecorder()
	a.ServeHTTP(response, r)
	if response.Code != http.StatusUnauthorized || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("auth %d", response.Code)
	}
	for _, tc := range []struct {
		method string
		ready  bool
		code   int
	}{{"POST", true, 405}, {"GET", false, 409}, {"GET", true, 200}} {
		response = httptest.NewRecorder()
		a.agentChoices(response, httptest.NewRequest(tc.method, "/agents/choices", nil), tc.ready)
		if response.Code != tc.code {
			t.Fatalf("%+v: %d", tc, response.Code)
		}
		if tc.code == 200 && (strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "bonnie.tasks") || !strings.Contains(response.Body.String(), `"selectable":true`)) {
			t.Fatalf("unsafe projection %s", response.Body)
		}
	}
}

func TestTargetedReportsFenceAndReplay(t *testing.T) {
	_, w, c := planningLocal(t)
	if err := w.moveCardTo(c.ID, "Building", 0, false, "selected"); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(c.ID)
	w.observeReport = fakeReportObserver
	request, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: a.TaskID, AgentID: "wrong", RunID: "run", AttemptID: "remote", TurnID: "turn", ToolCallID: "call"}, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 12})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.acceptReport(t.Context(), request); err == nil {
		t.Fatal("wrong report accepted before pin")
	}
	target := client.Target{TaskID: a.TaskID, AgentID: "selected", RunID: "run", AttemptID: "remote"}
	accepted := submitBuildReport(t, w, target, 12)
	ack, err := w.acceptReport(t.Context(), accepted)
	if err != nil || !ack.Duplicate {
		t.Fatalf("exact replay %+v %v", ack, err)
	}
	if _, err = w.acceptReport(t.Context(), request); err == nil {
		t.Fatal("wrong report accepted after pin")
	}
	c = workflowCard(t, w.s)
	if err = w.startPlanningTo(c.ID, 0, "planner"); err != nil {
		t.Fatal(err)
	}
	d := planningDetails(t, w, c.ID)
	out := planningOutcome(t, w, c.ID, "Reviewed plan")
	out.AgentID = "wrong"
	if err = planningDelivery(t, w, out); err == nil {
		t.Fatal("wrong planning report accepted")
	}
	ev := client.StatusEvent{Version: 1, Target: client.Target{TaskID: d.Turns[0].ID, AgentID: "wrong", RunID: "run", AttemptID: "remote"}, Seq: 1, State: runtime.RunRunning}
	if err = w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if err = w.result(t.Context(), client.Outcome{Version: 1, TaskID: d.Turns[0].ID, AgentID: "wrong", RunID: "run", AttemptID: "remote", State: runtime.RunFailed}); err != nil {
		t.Fatal(err)
	}
	d = planningDetails(t, w, c.ID)
	if d.Turns[0].AgentID != "" || d.Session.State != "queued" {
		t.Fatalf("wrong planner %+v", d)
	}
	if err = planningDelivery(t, w, planningOutcome(t, w, c.ID, "Reviewed plan")); err != nil {
		t.Fatal(err)
	}
}

func TestAssignmentRenderedMenusAndRevision(t *testing.T) {
	for _, lane := range []string{"Todo", "Planning", "Building", "Fix", "Done"} {
		t.Run(lane, func(t *testing.T) {
			c := Card{ID: "card", Title: "Card", Status: lane}
			d := pageData{uiSnapshot: uiSnapshot{cards: []Card{c}, planning: map[string]PlanningDetail{"card": {CanApprove: true, Session: PlanningSession{Revision: 2, Spec: "# Reviewed plan"}}}, attempts: []Attempt{{ID: "a", CardID: "card", Number: 1, Ready: true, State: "ready", PRURL: "https://github.com/example/repo/pull/1"}}}}
			var out strings.Builder
			if err := cardMenu(d, c).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			menu := out.String()
			for _, action := range []string{"Plan with agent", "Build with agent", "Fix with agent"} {
				want := (lane == "Todo" && action != "Fix with agent") || (lane == "Planning" && action == "Build with agent") || (lane == "Done" && action == "Fix with agent")
				if strings.Contains(menu, action) != want {
					t.Fatalf("%s %s: %s", lane, action, menu)
				}
			}
			out.Reset()
			if err := assignmentDialog(d).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			html := out.String()
			if lane == "Planning" && (!strings.Contains(html, "Reviewed plan") || !strings.Contains(html, `value="2"`) || !strings.Contains(html, "acknowledge")) {
				t.Fatal("missing rendered plan review")
			}
			if lane == "Todo" && strings.Contains(html, "acknowledge") {
				t.Fatal("unplanned build requires approval")
			}
		})
	}
}

func TestAssignmentHTTPValidationAndSharedWithoutPresence(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	a.workflow.presence = nil
	c := workflowCard(t, a.s)
	for _, form := range []url.Values{
		{"mode": {"unknown"}, "target_agent_id": {"selected"}},
		{"mode": {"build"}},
		{"mode": {"build"}, "target_agent_id": {"selected"}},
		{"mode": {"build"}, "target_agent_id": {"selected"}, "expected_revision": {"1"}},
	} {
		form.Set("_csrf", csrf.Value)
		form.Set("card_id", c.ID)
		status(t, request(a, http.MethodPost, "/cards/assign", form, session, csrf), http.StatusBadRequest)
	}
	assertActionLane(t, a.workflow, c.ProjectID, "Todo", c.ID)
	status(t, request(a, http.MethodGet, "/agents/choices", nil, session, csrf), http.StatusServiceUnavailable)
	status(t, request(a, http.MethodPost, "/cards/move", url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}, "status": {"Building"}}, session, csrf), http.StatusSeeOther)
}

func TestAssignmentUIFixture(t *testing.T) {
	d := pageData{csrf: "fixture-csrf", v: viewParams{View: viewBoard, Project: "p"}, uiSnapshot: uiSnapshot{projects: []Project{{ID: "p", Name: "Assignment fixtures"}}, planning: map[string]PlanningDetail{"planning": {CanApprove: true, Session: PlanningSession{Revision: 2, Spec: "# Reviewed specification\n\nImplement the approved change."}}}}}
	for _, c := range []Card{{ID: "todo", Title: "Direct build without planning", Status: "Todo"}, {ID: "planning", Title: "Review one revision before building", Status: "Planning"}, {ID: "ready", Title: "Fix the existing PR", Status: "Done"}} {
		c.ProjectID = "p"
		d.cards = append(d.cards, c)
	}
	d.attempts = []Attempt{{ID: "attempt", CardID: "ready", Number: 1, Ready: true, State: "ready", PRURL: "https://github.com/example/repo/pull/1"}}
	var out strings.Builder
	if err := appPage(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("JAWA_ASSIGNMENT_HTML"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTargetedReconciliationRejectsDifferentAgent(t *testing.T) {
	_, w, c := planningLocal(t)
	if err := w.moveCardTo(c.ID, "Building", 0, false, "selected"); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(c.ID)
	if err := w.event(t.Context(), client.StatusEvent{Version: 1, Target: client.Target{TaskID: a.TaskID, AgentID: "selected", RunID: "original", AttemptID: "original"}, Seq: 1, State: runtime.RunRunning}); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"wrong", "selected"} {
		out := client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: agent, RunID: agent + "-run", AttemptID: "remote", State: runtime.RunFailed, Error: "fixture"}
		tx, err := w.s.writeTx()
		if err != nil {
			t.Fatal(err)
		}
		if err = retainConflict(tx, out); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Reconcile(a.TaskID, "wrong-run", "remote"); err == nil {
		t.Fatal("reconciled different desired agent")
	}
	if err := w.Reconcile(a.TaskID, "selected-run", "remote"); err != nil {
		t.Fatal(err)
	}
	after, _ := w.CardResult(c.ID)
	if after.TargetAgentID != "selected" || after.AgentID != "selected" {
		t.Fatalf("reconciliation %+v", after)
	}
}

func TestTargetedOutboxRestartRetainsRouteAndPayload(t *testing.T) {
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	local := &Workflow{s: s, ctx: t.Context()}
	if err := local.moveCardTo(c.ID, "Building", 0, false, "offline"); err != nil {
		t.Fatal(err)
	}
	original, err := local.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := ""
	if err = s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	sub, err := nc.SubscribeSync("bonnie.tasks.>")
	if err != nil {
		t.Fatal(err)
	}
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
	w, err := NewWorkflow(t.Context(), reopened, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	msg, err := sub.NextMsg(3 * time.Second)
	if err != nil || msg.Subject != "bonnie.tasks.agent.offline" || string(msg.Data) != original.TaskJSON {
		t.Fatalf("restart route/payload %v %v", msg, err)
	}
	after, err := w.CardResult(c.ID)
	if err != nil || after.TargetAgentID != "offline" || after.TaskID != original.TaskID || after.AgentID != "" {
		t.Fatalf("restart identity %+v %v", after, err)
	}
}

func TestTargetedPlanningCannotReplaceExistingTurn(t *testing.T) {
	_, w, c := planningLocal(t)
	if err := w.startPlanningTo(c.ID, 0, "planner"); err != nil {
		t.Fatal(err)
	}
	if err := w.startPlanningTo(c.ID, 0, "other"); err == nil || !strings.Contains(err.Error(), "planning already started") {
		t.Fatalf("expected explicit rejection: %v", err)
	}
	if err := w.startPlanning(c.ID, 0); err != nil {
		t.Fatal(err)
	}
	d := planningDetails(t, w, c.ID)
	if len(d.Turns) != 1 || d.Turns[0].TargetAgentID != "planner" {
		t.Fatalf("replaced planning %+v", d.Turns)
	}
}

func TestTargetedPlanningHistoryLabel(t *testing.T) {
	_, w, c := planningLocal(t)
	if err := w.startPlanningTo(c.ID, 0, "planner"); err != nil {
		t.Fatal(err)
	}
	runs, err := w.s.runHistory(t.Context(), nil)
	if err != nil || len(runs) != 1 {
		t.Fatalf("history %v %v", runs, err)
	}
	if runs[0].label() != "Queued for planner" || runs[0].pinned() || !runs[0].matches("active") {
		t.Fatalf("queued target projection %+v", runs[0])
	}
}
