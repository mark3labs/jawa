package orchestrator

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
)

const stuckOutcome = `{"version":1,"task_id":"%s","agent_id":"agent","run_id":"run","attempt_id":"attempt","state":"running","error":"admitted answer resume interrupted"}`

// Every state an attempt can be left in when its agent never reports again
// must be releasable by Force stop, after which Retry, Reset and Delete work.
func TestForceStopReleasesEveryStuckBuildState(t *testing.T) {
	t.Parallel()
	identity := `agent_id='agent',run_id='run',remote_attempt_id='attempt'`
	for _, tc := range []struct {
		name  string
		setup string // applied to the latest attempt; %s is the task ID
		extra func(t *testing.T, w *Workflow, c Card, a Attempt)
	}{
		{name: "queued outbox never published", setup: `state='queued',published=0`},
		{name: "submitted but never picked up", setup: `state='submitted',published=1`},
		{name: "running on a vanished agent", setup: `published=1,state='running',run_state='running',` + identity},
		{name: "interrupted run", setup: `published=1,state='blocked',run_state='interrupted',error='agent run interrupted; awaiting agent recovery',` + identity},
		{name: "waiting with undeliverable guidance", setup: `published=1,state='blocked',run_state='waiting',` + identity, extra: func(t *testing.T, w *Workflow, c Card, a Attempt) {
			mustExec(t, w, `INSERT INTO typed_reports(task_id,report_id,tool_call_id,agent_id,run_id,attempt_id,turn_id,kind,request_json,accepted_at) VALUES(?,'blocker','call','agent','run','attempt','turn','report_blocker','{}',1)`, a.TaskID)
			mustExec(t, w, `INSERT INTO workflow_interactions(report_id,task_id,status,reply,created_at) VALUES('blocker',?,'queued','try again',1)`, a.TaskID)
		}},
		{name: "failed outcome stored with running snapshot", setup: `published=1,state='failed',run_state='running',outcome_json=printf('` + stuckOutcome + `',task_id),` + identity},
		{name: "completed status with lost outcome", setup: `published=1,state='blocked',run_state='completed',error='awaiting agent outcome',` + identity},
		{name: "resume pending on a vanished agent", setup: `published=1,state='running',run_state='running',error='Resume pending confirmation',` + identity, extra: func(t *testing.T, w *Workflow, c Card, a Attempt) {
			mustExec(t, w, `INSERT INTO failed_resumes(id,task_id,card_id,planning,request_json,previous_outcome,created_at) VALUES('resume',?,?,0,'{}','',1)`, a.TaskID, c.ID)
			mustExec(t, w, `INSERT INTO auto_resumes(task_id,card_id,planning,failure_token,due_at,next_check_at,state) VALUES(?,?,0,'token',1,1,'reserved')`, a.TaskID, c.ID)
		}},
		{name: "older attempt still active", setup: `published=1,state='failed',run_state='failed',outcome_json='{}'`, extra: func(t *testing.T, w *Workflow, c Card, a Attempt) {
			mustExec(t, w, `INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,state,run_state,published,created_at,updated_at) VALUES('old',?,0,'old-task','{}','running','running',1,0,0)`, c.ID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := actionWorkflow(t)
			c := workflowCard(t, w.s)
			if err := w.MoveCard(c.ID, "Building", 0); err != nil {
				t.Fatal(err)
			}
			a, err := w.CardResult(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, w, `UPDATE workflow_attempts SET `+tc.setup+` WHERE id=?`, a.ID)
			if tc.extra != nil {
				tc.extra(t, w, c, a)
			}
			card := Card{ID: c.ID, Status: "Building"}
			attempts, _ := w.Attempts()
			if canAction(card, attempts, "retry") || canAction(card, attempts, "reset") {
				t.Fatal("precondition: state is not stuck")
			}
			if !canAction(card, attempts, "force-stop") {
				t.Fatalf("force stop not offered: %+v", attempts)
			}
			if err := w.ForceStop(c.ID); err != nil {
				t.Fatal(err)
			}
			attempts, _ = w.Attempts()
			for _, a := range attempts {
				if attemptActive(a) {
					t.Fatalf("attempt still active: %+v", a)
				}
			}
			latest, _ := w.CardResult(c.ID)
			if runLabel(latest) != "Force-stopped" && tc.name != "older attempt still active" {
				t.Fatalf("label %q", runLabel(latest))
			}
			for _, action := range []string{"retry", "reset", "delete"} {
				if !canAction(card, attempts, action) {
					t.Fatalf("%s unavailable after force stop: %+v", action, latest)
				}
			}
			if canAction(card, attempts, "resume") || canAction(card, attempts, "force-stop") {
				t.Fatal("resume or force stop offered after force stop")
			}
			var pending, auto, open int
			_ = w.s.db.QueryRow(`SELECT count(*) FROM failed_resumes WHERE card_id=? AND status='pending'`, c.ID).Scan(&pending)
			_ = w.s.db.QueryRow(`SELECT count(*) FROM auto_resumes WHERE card_id=?`, c.ID).Scan(&auto)
			_ = w.s.db.QueryRow(`SELECT count(*) FROM workflow_interactions WHERE status IN ('pending','queued')`).Scan(&open)
			if pending+auto+open != 0 {
				t.Fatalf("leftover resume=%d auto=%d guidance=%d", pending, auto, open)
			}
			// Server-side guards agree with the UI.
			if err := w.Retry(c.ID); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func TestForceStopFencesLateDeliveries(t *testing.T) {
	t.Parallel()
	w := actionWorkflow(t)
	w.observeReport = fakeReportObserver
	c := workflowCard(t, w.s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(c.ID)
	ev := client.StatusEvent{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", EventID: "e1", Type: "run_state", Seq: 1, State: runtime.RunRunning}
	if err := w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if err := w.ForceStop(c.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := w.CardResult(c.ID)
	// The still-running agent keeps reporting: status, a terminal result, a
	// conflicting execution, and typed tool reports. None may revive it.
	ev.EventID, ev.Seq = "e2", 2
	if err := w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if err := w.result(t.Context(), client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", State: runtime.RunCompleted, Response: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := w.result(t.Context(), client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "other", RunID: "run2", AttemptID: "attempt2", State: runtime.RunFailed, Error: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []reportprotocol.Kind{reportprotocol.CompleteTask, reportprotocol.ReportBlocker} {
		p := reportprotocol.Payload{PRNumber: 7}
		if kind == reportprotocol.ReportBlocker {
			p = reportprotocol.Payload{Reason: "stuck"}
		}
		r, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "turn", ToolCallID: string(kind)}, kind, p)
		if err != nil {
			t.Fatal(err)
		}
		if ack, err := w.acceptReport(t.Context(), r); err == nil || ack.Accepted {
			t.Fatalf("%s accepted after force stop", kind)
		}
	}
	after, _ := w.CardResult(c.ID)
	if after.State != before.State || after.RunState != abandonedRunState || after.Error != forceStopNotice || after.OutcomeJSON != before.OutcomeJSON || after.Ready {
		t.Fatalf("force-stopped attempt changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if err := w.Reconcile(a.TaskID, "run2", "attempt2"); err == nil {
		t.Fatal("reconcile overrode a force stop")
	}
	var audits int
	_ = w.s.db.QueryRow(`SELECT count(*) FROM workflow_cancel_requests WHERE task_id=? AND status='abandoned'`, a.TaskID).Scan(&audits)
	if audits != 1 {
		t.Fatalf("audit rows %d", audits)
	}
}

func TestForceStopRefusesWhenNothingIsStuck(t *testing.T) {
	t.Parallel()
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	if err := w.ForceStop(c.ID); err == nil {
		t.Fatal("force stop on a Todo card with no attempts")
	}
	actionAttempt(t, w, c, "failed", "failed")
	if err := w.ForceStop(c.ID); err == nil || !strings.Contains(err.Error(), "nothing to force stop") {
		t.Fatalf("terminal attempt: %v", err)
	}
	if err := w.ForceStop("missing"); err == nil {
		t.Fatal("missing card")
	}
}

// Cancel planning is the Planning escape hatch and must never wait on an agent.
func TestCancelPlanningAlwaysLeavesPlanning(t *testing.T) {
	t.Parallel()
	t.Run("pending resume", func(t *testing.T) {
		t.Parallel()
		s, w, c := planningLocal(t)
		if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
			t.Fatal(err)
		}
		mustExec(t, w, `INSERT INTO failed_resumes(id,task_id,card_id,planning,request_json,previous_outcome,created_at) VALUES('resume','turn',?,1,'{}','',1)`, c.ID)
		if err := w.CancelPlanning(c.ID); err != nil {
			t.Fatal(err)
		}
		assertCardStatus(t, w, c.ID, "Todo")
		var pending int
		_ = s.db.QueryRow(`SELECT count(*) FROM failed_resumes WHERE status='pending'`).Scan(&pending)
		if pending != 0 {
			t.Fatal("pending resume left behind")
		}
	})
	t.Run("no active session", func(t *testing.T) {
		t.Parallel()
		_, w, c := planningLocal(t)
		mustExec(t, w, `UPDATE cards SET status='Planning' WHERE id=?`, c.ID)
		if err := w.CancelPlanning(c.ID); err != nil {
			t.Fatal(err)
		}
		assertCardStatus(t, w, c.ID, "Todo")
	})
	t.Run("force stop points to cancel planning", func(t *testing.T) {
		t.Parallel()
		_, w, c := planningLocal(t)
		mustExec(t, w, `UPDATE cards SET status='Planning' WHERE id=?`, c.ID)
		if err := w.ForceStop(c.ID); err == nil || !strings.Contains(err.Error(), "Cancel planning") {
			t.Fatalf("%v", err)
		}
	})
}

func TestForceStopCommandResolvesBoardCodes(t *testing.T) {
	t.Parallel()
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{c.ID, c.ID[:8], "JAW-" + strings.ToUpper(c.ID[:4]), "jaw-" + c.ID[:6]} {
		id, status, err := resolveCard(w.s, ref)
		if err != nil || id != c.ID || status != "Building" {
			t.Fatalf("%s: %s %s %v", ref, id, status, err)
		}
	}
	for _, ref := range []string{"", "JAW-", "%", "zzzz"} {
		if _, _, err := resolveCard(w.s, ref); err == nil {
			t.Fatalf("%q resolved", ref)
		}
	}
	cmd := ForceStopCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--card", c.ID})
	if err := cmd.ExecuteContext(t.Context()); err == nil {
		t.Fatal("missing data dir accepted")
	}
}

func TestForceStopHTTPRequiresAcknowledgement(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, csrf := sseCookies(t, a)
	c := workflowCard(t, a.s)
	if err := a.workflow.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}}
	body := assertSSE(t, sseRequest(a, "POST", "/cards/force-stop", form, session, csrf))
	if !strings.Contains(body, "confirm") || !strings.Contains(body, `"forceOpen":false`) {
		t.Fatal(body)
	}
	if latest, _ := a.workflow.CardResult(c.ID); latest.RunState == abandonedRunState {
		t.Fatal("force stopped without acknowledgement")
	}
	form.Set("acknowledge", "yes")
	body = assertSSE(t, sseRequest(a, "POST", "/cards/force-stop", form, session, csrf))
	if !strings.Contains(body, "force-stopped") || !strings.Contains(body, `"forceOpen":false`) {
		t.Fatal(body)
	}
	if latest, _ := a.workflow.CardResult(c.ID); latest.RunState != abandonedRunState {
		t.Fatalf("not force stopped: %+v", latest)
	}
}

func mustExec(t *testing.T, w *Workflow, query string, args ...any) {
	t.Helper()
	if _, err := w.s.db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func assertCardStatus(t *testing.T, w *Workflow, id, want string) {
	t.Helper()
	var got string
	if err := w.s.db.QueryRow(`SELECT status FROM cards WHERE id=?`, id).Scan(&got); err != nil || got != want {
		t.Fatalf("card status %q want %q (%v)", got, want, err)
	}
}
