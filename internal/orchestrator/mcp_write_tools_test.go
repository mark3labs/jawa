package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bonnie "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/nats-io/nats.go"
)

var lifecycleTools = []string{"create_project", "create_card", "set_auto_merge", "start_planning", "reply_planning", "approve_plan", "retry_planning", "cancel_planning", "start_build", "retry_build", "resume_failed", "guide_blocked", "cancel_run", "reset_card", "move_card"}

func mcpSetup(t *testing.T, w *Workflow) {
	t.Helper()
	if _, err := w.s.db.Exec(`INSERT INTO settings(key,value) VALUES('nats_configured','true')`); err != nil {
		t.Fatal(err)
	}
}
func mcpWorkflowClient(t *testing.T, w *Workflow, issueClient *http.Client) *client.Client {
	t.Helper()
	_, secret, err := w.s.CreateAPIKey(t.Context(), "lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&mcpService{s: w.s, w: w, issueClient: issueClient}).handler())
	t.Cleanup(srv.Close)
	mcpSetup(t, w)
	return mcpTestClient(t, srv.URL, secret)
}
func mcpReject(t *testing.T, c *client.Client, tool string, args map[string]any, contains string) {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = tool
	req.Params.Arguments = args
	result, err := c.CallTool(t.Context(), req)
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	b, _ := json.Marshal(result.Content)
	if !result.IsError || !strings.Contains(string(b), contains) {
		t.Fatalf("%s: %s", tool, b)
	}
}
func mcpActionArgs(t *testing.T, c *client.Client, id, tool string) map[string]any {
	t.Helper()
	card := mcpDecode[mcpCard](t, mcpCall(t, c, "get_card", map[string]any{"card_id": id}))
	for _, a := range card.Actions {
		if a.Tool == tool {
			return a.Args
		}
	}
	t.Fatalf("missing %s action: %+v", tool, card)
	return nil
}
func mcpResultCard(t *testing.T, b []byte) mcpCard {
	t.Helper()
	return mcpDecode[struct {
		Card mcpCard `json:"card"`
	}](t, b).Card
}
func TestMCPWriteToolsRequireNATSSetup(t *testing.T) {
	w, url, secret := mcpTestServer(t)
	c := mcpTestClient(t, url, secret)
	for _, tool := range lifecycleTools {
		mcpReject(t, c, tool, map[string]any{"card_id": "absent", "name": "new", "title": "new", "project_id": "absent", "idempotency_key": "key", "mode": "on", "expected_mode": "inherit", "reply": "hello", "message": "hello", "expected_interaction": "token", "revision": 1, "expected_resume": "token", "position": 0}, "NATS authentication setup")
	}
	cards, err := w.s.Cards("")
	if err != nil || len(cards) != 0 {
		t.Fatal(cards, err)
	}
	projects, err := w.s.Projects()
	if err != nil || len(projects) != 0 {
		t.Fatal(projects, err)
	}
	mcpCall(t, c, "list_projects", nil)
}
func TestMCPLifecycleAnnotations(t *testing.T) {
	_, url, secret := mcpTestServer(t)
	c := mcpTestClient(t, url, secret)
	listed, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range lifecycleTools {
		found := false
		for _, tool := range listed.Tools {
			if tool.Name != name {
				continue
			}
			found = true
			destructive := name == "cancel_run" || name == "cancel_planning" || name == "reset_card"
			if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != destructive {
				t.Fatalf("%s destructive annotation", name)
			}
			if (name == "create_card" || name == "create_project") && (tool.Annotations.IdempotentHint == nil || !*tool.Annotations.IdempotentHint) {
				t.Fatalf("%s idempotent annotation", name)
			}
		}
		if !found {
			t.Fatal("missing tool", name)
		}
	}
}

func TestMCPCreateProjectAndCardIdempotent(t *testing.T) {
	w, url, secret := mcpTestServer(t)
	mcpSetup(t, w)
	c := mcpTestClient(t, url, secret)
	pa := map[string]any{"name": "project", "provider": "github", "repo_name": "acme/repo", "idempotency_key": "project"}
	first := mcpDecode[struct {
		Project  mcpProject `json:"project"`
		Replayed bool       `json:"replayed"`
	}](t, mcpCall(t, c, "create_project", pa))
	second := mcpDecode[struct {
		Project  mcpProject `json:"project"`
		Replayed bool       `json:"replayed"`
	}](t, mcpCall(t, c, "create_project", pa))
	if first.Project.ID != second.Project.ID || first.Replayed || !second.Replayed || first.Project.Repo != "https://github.com/acme/repo" {
		t.Fatal(first, second)
	}
	args := map[string]any{"project_id": first.Project.ID, "title": "task", "idempotency_key": "card"}
	one := mcpResultCard(t, mcpCall(t, c, "create_card", args))
	two := mcpResultCard(t, mcpCall(t, c, "create_card", args))
	if one.ID != two.ID {
		t.Fatal("duplicate card")
	}
	args["title"] = "changed"
	mcpReject(t, c, "create_card", args, errIdempotencyConflict.Error())
	args["title"] = "task"
	for _, key := range []string{"", strings.Repeat("a", 129), "a b"} {
		args["idempotency_key"] = key
		mcpReject(t, c, "create_card", args, "idempotency_key")
	}
	args["idempotency_key"] = "other"
	mcpCall(t, c, "create_card", args)
	cards, _ := w.s.Cards("")
	projects, _ := w.s.Projects()
	if len(cards) != 2 || len(projects) != 1 {
		t.Fatal(cards, projects)
	}
	if _, err := w.s.db.Exec(`DELETE FROM cards WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	args["idempotency_key"] = "card"
	mcpReject(t, c, "create_card", args, "no longer exists")
}
func TestMCPCreateCardWithIssue(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "uncertain"}[uncertain], func(t *testing.T) {
			w := actionWorkflow(t)
			p, err := w.s.CreateProjectDetails("project", "github", "https://github.com/acme/repo", "master")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("JAWA_GITHUB_TOKEN", "fixture")
			calls := 0
			stub := &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if uncertain {
					return nil, errors.New("lost response")
				}
				r := issueResponse(`{"number":17}`)
				r.StatusCode = 201
				return r, nil
			})}
			c := mcpWorkflowClient(t, w, stub)
			args := map[string]any{"project_id": p.ID, "title": "task", "idempotency_key": "issue", "create_issue": true}
			if uncertain {
				mcpReject(t, c, "create_card", args, "Issue creation")
				delete(args, "create_issue")
				args["issue_recovery"] = "unlinked"
				args["title"] = "changed"
				mcpReject(t, c, "create_card", args, "different arguments")
				args["title"] = "task"
			}
			one := mcpResultCard(t, mcpCall(t, c, "create_card", args))
			two := mcpResultCard(t, mcpCall(t, c, "create_card", args))
			if one.ID != two.ID || calls != 1 || (!uncertain && one.IssueURL != p.Repo+"/issues/17") || (uncertain && one.IssueURL != "") {
				t.Fatal(one, two, calls)
			}
			args["create_issue"] = true
			args["issue_recovery"] = "unlinked"
			mcpReject(t, c, "create_card", args, "not both")
			delete(args, "create_issue")
			delete(args, "issue_recovery")
			mcpReject(t, c, "create_card", args, "different arguments")
		})
	}
}
func TestMCPPlanningLifecycle(t *testing.T) {
	w, url, secret := mcpTestServer(t)
	mcpSetup(t, w)
	c := mcpTestClient(t, url, secret)
	card := workflowCard(t, w.s)
	started := mcpResultCard(t, mcpCall(t, c, "start_planning", map[string]any{"card_id": card.ID}))
	if started.Lane != "Planning" || len(started.Actions) != 3 {
		t.Fatal(started)
	}
	mcpReject(t, c, "start_build", map[string]any{"card_id": card.ID}, "approve_plan")
	if err := planningDelivery(t, w, planningOutcome(t, w, card.ID, "Draft", "Which API?")); err != nil {
		t.Fatal(err)
	}
	args := mcpActionArgs(t, c, card.ID, "reply_planning")
	token := args["expected_interaction"]
	args["reply"] = "v2"
	args["expected_interaction"] = "wrong"
	mcpReject(t, c, "reply_planning", args, "Unable to apply request")
	args["expected_interaction"] = token
	mcpCall(t, c, "reply_planning", args)
	mcpReject(t, c, "reply_planning", args, "Unable to apply request")
	if err := planningDelivery(t, w, planningOutcome(t, w, card.ID, "Final")); err != nil {
		t.Fatal(err)
	}
	args = mcpActionArgs(t, c, card.ID, "approve_plan")
	rev := args["revision"].(float64)
	args["revision"] = rev - 1
	mcpReject(t, c, "approve_plan", args, "revision")
	args["revision"] = rev
	built := mcpResultCard(t, mcpCall(t, c, "approve_plan", args))
	if built.Lane != "Building" {
		t.Fatal(built)
	}
	if _, err := w.CardResult(card.ID); err != nil {
		t.Fatal(err)
	}
}
func TestMCPPlanningFailureRetryAndResume(t *testing.T) {
	for _, tool := range []string{"retry_planning", "resume_failed"} {
		t.Run(tool, func(t *testing.T) {
			w, url, secret := mcpTestServer(t)
			mcpSetup(t, w)
			c := mcpTestClient(t, url, secret)
			card := workflowCard(t, w.s)
			mcpCall(t, c, "start_planning", map[string]any{"card_id": card.ID})
			out := planningOutcome(t, w, card.ID, "")
			out.State = runtime.RunFailed
			out.Error = "fixture"
			mcpResumeControl(t, w, out)
			if err := w.result(t.Context(), out); err != nil {
				t.Fatal(err)
			}
			mcpActionArgs(t, c, card.ID, "retry_planning")
			args := mcpActionArgs(t, c, card.ID, "resume_failed")
			token := args["expected_resume"]
			args["expected_resume"] = "wrong"
			mcpReject(t, c, "resume_failed", args, "Unable to apply request")
			args["expected_resume"] = token
			if tool == "retry_planning" {
				args = mcpActionArgs(t, c, card.ID, tool)
			}
			mcpCall(t, c, tool, args)
			mcpReject(t, c, tool, args, "Unable to apply request")
		})
	}
}
func TestMCPBuildControls(t *testing.T) {
	t.Run("queue and reorder", func(t *testing.T) {
		w, url, secret := mcpTestServer(t)
		mcpSetup(t, w)
		c := mcpTestClient(t, url, secret)
		card := workflowCard(t, w.s)
		other, err := w.s.CreateCard(card.ProjectID, "other", "")
		if err != nil {
			t.Fatal(err)
		}
		mcpCall(t, c, "move_card", map[string]any{"card_id": other.ID, "position": 0})
		assertActionLane(t, w, card.ProjectID, "Todo", other.ID, card.ID)
		mcpReject(t, c, "move_card", map[string]any{"card_id": card.ID, "position": -1}, "invalid card position")
		built := mcpResultCard(t, mcpCall(t, c, "start_build", map[string]any{"card_id": card.ID}))
		if built.Lane != "Building" {
			t.Fatal(built)
		}
		found := false
		for _, u := range built.Unavailable {
			if u.Tool == "reset_card" && u.Reason != "" {
				found = true
			}
		}
		if !found {
			t.Fatal(built)
		}
		mcpReject(t, c, "reset_card", map[string]any{"card_id": card.ID}, "Unable to apply request")
		mcpCall(t, c, "set_auto_merge", map[string]any{"card_id": card.ID, "mode": "off", "expected_mode": "inherit"})
		mcpReject(t, c, "set_auto_merge", map[string]any{"card_id": card.ID, "mode": "on", "expected_mode": "inherit"}, "Unable to apply request")
	})
	t.Run("guidance", func(t *testing.T) {
		w, card, _ := parkedBuild(t)
		c := mcpWorkflowClient(t, w, nil)
		args := mcpActionArgs(t, c, card.ID, "guide_blocked")
		args["message"] = "Use v2"
		mcpCall(t, c, "guide_blocked", args)
		mcpReject(t, c, "guide_blocked", args, "Unable to apply request")
	})
	for _, tool := range []string{"retry_build", "resume_failed", "reset_card"} {
		t.Run(tool, func(t *testing.T) {
			w := actionWorkflow(t)
			card := workflowCard(t, w.s)
			if err := w.MoveCard(card.ID, "Building", 0); err != nil {
				t.Fatal(err)
			}
			a, err := w.CardResult(card.ID)
			if err != nil {
				t.Fatal(err)
			}
			out := bonnie.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed, Error: "fixture"}
			if err := w.result(t.Context(), out); err != nil {
				t.Fatal(err)
			}
			mcpResumeControl(t, w, out)
			c := mcpWorkflowClient(t, w, nil)
			args := mcpActionArgs(t, c, card.ID, tool)
			if tool == "resume_failed" {
				token := args["expected_resume"]
				args["expected_resume"] = "wrong"
				mcpReject(t, c, tool, args, "Unable to apply request")
				args["expected_resume"] = token
			}
			mcpCall(t, c, tool, args)
		})
	}
}
func TestMCPCancelRunAndPlanning(t *testing.T) {
	s, nc := workflowFixture(t)
	cl, err := bonnie.New(nc, bonnie.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context(), client: cl}
	c := mcpWorkflowClient(t, w, nil)
	card := workflowCard(t, s)
	mcpCall(t, c, "start_planning", map[string]any{"card_id": card.ID})
	cancelled := mcpResultCard(t, mcpCall(t, c, "cancel_planning", mcpActionArgs(t, c, card.ID, "cancel_planning")))
	if cancelled.Lane != "Todo" {
		t.Fatal(cancelled)
	}
	mcpCall(t, c, "start_build", mcpActionArgs(t, c, card.ID, "start_build"))
	a, err := w.CardResult(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := bonnie.Target{TaskID: a.TaskID, AgentID: "cancel-agent", RunID: "run", AttemptID: "remote"}
	ev := bonnie.StatusEvent{Version: 1, Target: target, Type: "run_state", EventID: "running", Seq: 1, State: runtime.RunRunning}
	if err = w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if err = replyJSON(nc, "bonnie.queries.cancel-agent", bonnie.Status{Version: 1, Target: target, State: runtime.RunRunning, Active: true, TurnID: "turn"}); err != nil {
		t.Fatal(err)
	}
	sub, err := nc.Subscribe("bonnie.commands.cancel-agent", func(msg *nats.Msg) {
		raw, _ := json.Marshal(bonnie.Status{Version: 1, Target: target, State: runtime.RunRunning, TurnID: "turn", CancelStatus: "requested"})
		_ = msg.Respond(raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
	result := mcpDecode[struct {
		Note string  `json:"note"`
		Card mcpCard `json:"card"`
	}](t, mcpCall(t, c, "cancel_run", mcpActionArgs(t, c, card.ID, "cancel_run")))
	if !strings.Contains(result.Note, "not confirmed") || result.Card.Lane != "Building" {
		t.Fatal(result)
	}
}

func TestMCPAgentTargeting(t *testing.T) {
	s, nc := workflowFixture(t)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	registerChoice(t, w, "agent-1", true)
	c := mcpWorkflowClient(t, w, nil)
	card := workflowCard(t, s)
	mcpReject(t, c, "start_build", map[string]any{"card_id": card.ID, "agent": "missing"}, "selected agent")
	mcpCall(t, c, "start_build", map[string]any{"card_id": card.ID, "agent": "agent-1"})
	a, err := w.CardResult(card.ID)
	if err != nil || a.TargetAgentID != "agent-1" {
		t.Fatal(a, err)
	}
}

// Compare the rendered dashboard menu, not a second copy of its state predicates.
func TestMCPActionsMatchDashboardMenu(t *testing.T) {
	routes := map[string]string{"/cards/plan": "start_planning", "/cards/move": "start_build", "/cards/retry": "retry_build", "/cards/resume": "resume_failed", "/cards/cancel": "cancel_run", "/cards/reset": "reset_card", "/cards/planning/retry": "retry_planning", "/cards/planning/cancel": "cancel_planning"}
	for _, state := range []string{"Todo", "Planning-ready", "Building-failed", "Done-ready"} {
		t.Run(state, func(t *testing.T) {
			w := actionWorkflow(t)
			card := workflowCard(t, w.s)
			switch state {
			case "Planning-ready":
				if err := w.MoveCard(card.ID, "Planning", 0); err != nil {
					t.Fatal(err)
				}
				if err := planningDelivery(t, w, planningOutcome(t, w, card.ID, "Final")); err != nil {
					t.Fatal(err)
				}
			case "Building-failed":
				actionAttempt(t, w, card, "failed", "failed")
			case "Done-ready":
				a := actionAttempt(t, w, card, "completed", "completed")
				if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET ready=1 WHERE id=?; UPDATE cards SET status='Done' WHERE id=?`, a.ID, card.ID); err != nil {
					t.Fatal(err)
				}
			}
			m := &mcpService{s: w.s, w: w}
			d, err := m.snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			card, _ = selectedCard(d.cards, card.ID)
			var html bytes.Buffer
			if err := cardMenu(d, card).Render(t.Context(), &html); err != nil {
				t.Fatal(err)
			}
			acts, _ := cardActions(d, card)
			actual := map[string]bool{}
			for _, a := range acts {
				actual[a.Tool] = true
			}
			for route, tool := range routes {
				// Planning approval/replies live on the plan page, not the menu.
				if route == "/cards/move" && state != "Todo" {
					continue
				}
				rendered := strings.Contains(html.String(), "'"+route+"'") || strings.Contains(html.String(), "&#39;"+route+"&#39;") || strings.Contains(html.String(), "\""+route+"\"")
				if rendered != actual[tool] {
					t.Fatalf("%s rendered=%v action=%v: %s", tool, rendered, actual[tool], html.String())
				}
			}
		})
	}
}

// Attach a real BONNIE query client without running dispatch loops.
func mcpResumeControl(t *testing.T, w *Workflow, out bonnie.Outcome) {
	t.Helper()
	_, nc := workflowFixture(t)
	var err error
	w.client, err = bonnie.New(nc, bonnie.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	w.resumeConn = nc
	target := bonnie.Target{TaskID: out.TaskID, AgentID: out.AgentID, RunID: out.RunID, AttemptID: out.AttemptID}
	sub, err := nc.Subscribe("bonnie.queries."+out.AgentID, func(msg *nats.Msg) {
		raw, _ := json.Marshal(bonnie.Status{Version: 1, Target: target, State: runtime.RunFailed, TurnID: "turn", Seq: 100})
		_ = msg.Respond(raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
}
