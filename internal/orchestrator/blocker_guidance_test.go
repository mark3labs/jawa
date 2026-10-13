package orchestrator

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
)

// parkedBuild accepts a build blocker and delivers its trusted waiting outcome.
func parkedBuild(t *testing.T) (*Workflow, Card, reportprotocol.Request) {
	t.Helper()
	_, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(c.ID)
	w.observeReport = fakeReportObserver
	r := reportRequest(t, a.TaskID, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "Need credentials"})
	if ack, err := w.acceptReport(t.Context(), r); err != nil || !ack.Accepted || !ack.AwaitGuidance {
		t.Fatal(ack, err)
	}
	if err := w.result(t.Context(), waitingOutcome(r)); err != nil {
		t.Fatal(err)
	}
	return w, c, r
}

func waitingOutcome(r reportprotocol.Request) client.Outcome {
	return client.Outcome{Version: 1, TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID, State: runtime.RunWaiting, AnswerSubject: "bonnie.answers." + r.AgentID, Suspend: &runtime.SuspendRequest{Kind: runtime.SuspendQuestion, Prompt: r.Payload.Reason, TurnID: r.TurnID, ToolCallID: r.ToolCallID}}
}

func statusEvent(r reportprotocol.Request, state runtime.RunState, seq int) client.StatusEvent {
	return client.StatusEvent{Version: 1, Type: "run_state", EventID: string(state) + string(rune('a'+seq)), TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID, State: state, Seq: seq}
}

func TestBlockerParksRunAndSurvivesStatusUpdates(t *testing.T) {
	t.Parallel()
	w, c, r := parkedBuild(t)
	a, _ := w.CardResult(c.ID)
	if a.State != "blocked" || a.RunState != "waiting" || a.Error != "Need credentials" || a.OutcomeJSON != "" || terminalFailure(a) || runLabel(a) != "Blocked" {
		t.Fatal(a)
	}
	var waiting string
	if err := w.s.db.QueryRow(`SELECT waiting_json FROM workflow_interactions WHERE report_id=?`, r.ReportID).Scan(&waiting); err != nil || waiting == "" {
		t.Fatal("waiting outcome not stored", err)
	}
	// Late running/waiting events must not erase the blocker.
	for i, state := range []runtime.RunState{runtime.RunRunning, runtime.RunWaiting} {
		if err := w.event(t.Context(), statusEvent(r, state, i+1)); err != nil {
			t.Fatal(err)
		}
		if a, _ = w.CardResult(c.ID); a.State != "blocked" || a.Error != "Need credentials" || runLabel(a) != "Blocked" {
			t.Fatal(state, a)
		}
	}
	// A second report cannot race an open blocker.
	second, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID, TurnID: "turn", ToolCallID: "other"}, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 3})
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := w.acceptReport(t.Context(), second); err == nil || ack.Accepted {
		t.Fatal("report accepted while waiting for guidance", ack)
	}
	// Queued guidance is visible until delivery.
	if err := w.GuideBlocked(c.ID, "Use the vault", r.ReportID); err != nil {
		t.Fatal(err)
	}
	if a, _ = w.CardResult(c.ID); a.Error != guidanceQueuedNotice {
		t.Fatal(a)
	}
	if err := w.event(t.Context(), statusEvent(r, runtime.RunWaiting, 3)); err != nil {
		t.Fatal(err)
	}
	if a, _ = w.CardResult(c.ID); a.State != "blocked" || a.Error != guidanceQueuedNotice {
		t.Fatal(a)
	}
	// Cancelling the parked run closes the interaction and restores fresh Retry.
	if err := w.event(t.Context(), statusEvent(r, runtime.RunCancelled, 4)); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := w.s.db.QueryRow(`SELECT status FROM workflow_interactions WHERE report_id=?`, r.ReportID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatal(status, err)
	}
	if a, _ = w.CardResult(c.ID); a.State != "failed" || !terminalFailure(a) {
		t.Fatal(a)
	}
	if err := w.GuideBlocked(c.ID, "late", r.ReportID); err == nil {
		t.Fatal("guidance accepted for a cancelled run")
	}
	if err := w.Retry(c.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAnsweredBlockerIgnoresStaleWaitingReplay(t *testing.T) {
	t.Parallel()
	w, c, r := parkedBuild(t)
	// Emulate delivered guidance: the run continues in the same conversation.
	if _, err := w.s.db.Exec(`UPDATE workflow_interactions SET status='answered' WHERE report_id=?`, r.ReportID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET state='running',run_state='running',error='' WHERE task_id=?`, r.TaskID); err != nil {
		t.Fatal(err)
	}
	// Result recovery replays retained outcomes; the old suspension is stale.
	if err := w.result(t.Context(), waitingOutcome(r)); err != nil {
		t.Fatal(err)
	}
	if a, _ := w.CardResult(c.ID); a.State != "running" || a.OutcomeJSON != "" {
		t.Fatal("stale waiting outcome re-parked the run", a)
	}
	// The continued run may block again in the same task.
	next, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID, TurnID: "turn", ToolCallID: "call-2"}, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "Vault is sealed"})
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := w.acceptReport(t.Context(), next); err != nil || !ack.Accepted || !ack.AwaitGuidance {
		t.Fatal(ack, err)
	}
	if a, _ := w.CardResult(c.ID); a.State != "blocked" || a.Error != "Vault is sealed" {
		t.Fatal(a)
	}
}

func TestBlockerFromAgentThatCannotParkStaysRetryable(t *testing.T) {
	t.Parallel()
	_, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(c.ID)
	w.observeReport = fakeReportObserver
	r := reportRequest(t, a.TaskID, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "Need credentials"})
	if _, err := w.acceptReport(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	// An older host ignores AwaitGuidance and completes the run instead.
	out := client.Outcome{Version: 1, TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID, State: runtime.RunCompleted, Response: "Typed report committed."}
	if err := w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	a, _ = w.CardResult(c.ID)
	if a.State != "blocked" || a.Error != "Need credentials" || !terminalFailure(a) {
		t.Fatal(a)
	}
	if guidance, err := w.s.blockerGuidance(); err != nil || len(guidance) != 0 {
		t.Fatal(guidance, err)
	}
	if err := w.Retry(c.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPostApprovalPlanningBlockerStaysTerminal(t *testing.T) {
	t.Parallel()
	_, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	d := planningDetails(t, w, c.ID)
	if _, err := w.s.db.Exec(`UPDATE planning_sessions SET state='approved' WHERE id=?`, d.Session.ID); err != nil {
		t.Fatal(err)
	}
	w.observeReport = fakeReportObserver
	r := reportRequest(t, d.Turns[0].ID, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "Planning done; build handoff required"})
	ack, err := w.acceptReport(t.Context(), r)
	if err != nil || !ack.Accepted || ack.AwaitGuidance {
		t.Fatal("post-approval planning blocker must not park the planner", ack, err)
	}
	tx, err := w.s.writeTx()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if terminal, err := acceptedReport(tx, r.TaskID); err != nil || !terminal {
		t.Fatal(terminal, err)
	}
}

func TestBlockerGuidanceCardUI(t *testing.T) {
	t.Parallel()
	attempt := Attempt{CardID: "card", TaskID: "task", AgentID: "agent", RunID: "run", RemoteAttemptID: "remote", State: "blocked", RunState: "waiting", Error: "Need credentials"}
	for _, tc := range []struct {
		name, lane string
		queued     bool
		reply      bool
	}{
		{name: "building", lane: "Building", reply: true},
		{name: "fix", lane: "Fix", reply: true},
		{name: "queued", lane: "Building", queued: true},
		{name: "done", lane: "Done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := Card{ID: "card", Title: "Card", Status: tc.lane}
			d := pageData{csrf: "csrf-token", v: viewParams{View: viewBoard, Project: "project"}, attempts: []Attempt{attempt}, planning: map[string]PlanningDetail{},
				blockers: map[string]BlockerGuidance{"card": {CardID: "card", TaskID: "task", Token: "report-token", Reason: "Need credentials", Queued: tc.queued}}}
			var buf bytes.Buffer
			if err := cardMenu(d, card).Render(t.Context(), &buf); err != nil {
				t.Fatal(err)
			}
			if err := cardNote(d, card).Render(t.Context(), &buf); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			replies := strings.Count(out, "Reply with guidance")
			if tc.reply != (replies == 2) || (!tc.reply && replies != 0) {
				t.Fatalf("reply actions = %d\n%s", replies, out)
			}
			if tc.reply && !strings.Contains(out, `$guideToken = &#34;report-token&#34;`) && !strings.Contains(out, `$guideToken = "report-token"`) {
				t.Fatal("guidance action not bound to the blocker", out)
			}
			if tc.lane != "Done" && !strings.Contains(out, "Need credentials") {
				t.Fatal("blocker reason hidden", out)
			}
			if tc.queued && !strings.Contains(out, guidanceQueuedNotice) {
				t.Fatal("queued guidance not shown", out)
			}
			// A parked run can always be abandoned.
			if tc.lane != "Done" && !strings.Contains(out, "Cancel run…") {
				t.Fatal("parked run cannot be cancelled", out)
			}
		})
	}
}

func TestBlockerGuidanceHTTP(t *testing.T) {
	t.Parallel()
	app := testApp(t)
	session, csrf := setupTest(t, app)
	w := app.workflow
	card := workflowCard(t, w.s)
	if err := w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(card.ID)
	w.observeReport = fakeReportObserver
	r := reportRequest(t, a.TaskID, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "Need credentials"})
	if ack, err := w.acceptReport(t.Context(), r); err != nil || !ack.AwaitGuidance {
		t.Fatal(ack, err)
	}
	form := url.Values{"_csrf": {csrf.Value}, "view": {"board"}, "card_id": {card.ID}, "message": {"Use the vault"}}
	for _, bad := range []string{"", "stale"} {
		form.Set("expected_interaction", bad)
		status(t, request(app, "POST", "/cards/guide", form, session, csrf), http.StatusBadRequest)
	}
	form.Set("expected_interaction", r.ReportID)
	status(t, request(app, "POST", "/cards/guide", form, session, csrf), http.StatusSeeOther)
	status(t, request(app, "POST", "/cards/guide", form, session, csrf), http.StatusBadRequest)
	var state, reply string
	if err := w.s.db.QueryRow(`SELECT status,reply FROM workflow_interactions WHERE report_id=?`, r.ReportID).Scan(&state, &reply); err != nil || state != "queued" || reply != "Use the vault" {
		t.Fatal(state, reply, err)
	}
}
