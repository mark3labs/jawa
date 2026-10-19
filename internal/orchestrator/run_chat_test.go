package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
)

func TestRunChatDurableAdmissionAndReplay(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	attempt := pinnedDetailAttempt(t, a)
	id := uuid.NewString()
	form := url.Values{"_csrf": {csrf.Value}, "message_id": {id}, "text": {"durable offline message"}, "agent_id": {"malicious-agent"}, "run_id": {"malicious-run"}}
	status(t, request(a, "POST", detailAction(runDetail{Record: buildRun(attempt), Tab: "chat"}, "chat"), form, session, csrf), http.StatusSeeOther)
	messages := a.workflow.chatMessages(buildRun(attempt))
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	m := messages[0]
	if m.AgentID != attempt.AgentID || m.RunID != attempt.RunID || m.Status != "queued" || m.Text != form.Get("text") {
		t.Fatal(m)
	}
	again, err := a.workflow.QueueRunChat(t.Context(), "Build", attempt.ID, id, m.Text, "")
	if err != nil || again.ID != m.ID {
		t.Fatal(again, err)
	}
	if _, err = a.workflow.QueueRunChat(t.Context(), "Build", attempt.ID, id, "changed", ""); err == nil {
		t.Fatal("conflicting replay accepted")
	}
	// A new workflow object reads the committed queue without remote availability.
	restarted := &Workflow{s: a.s, ctx: t.Context()}
	if got := restarted.chatMessages(buildRun(attempt)); len(got) != 1 || got[0].ID != m.ID {
		t.Fatal(got)
	}
	for _, text := range []string{" \n", strings.Repeat("é", 32001), string([]byte{0xff})} {
		if _, err = a.workflow.QueueRunChat(t.Context(), "Build", attempt.ID, uuid.NewString(), text, ""); err == nil {
			t.Fatal("invalid text accepted")
		}
	}
	if _, err = a.workflow.QueueRunChat(t.Context(), "Build", attempt.ID, uuid.NewString(), strings.Repeat("é", 32000), ""); err != nil {
		t.Fatal(err)
	}
}

func TestRunChatClassifiesAllPinnedStates(t *testing.T) {
	for _, state := range []string{"running", "waiting", "failed", "interrupted", "cancelled", "completed", "retired"} {
		t.Run(state, func(t *testing.T) {
			a := testApp(t)
			attempt := pinnedDetailAttempt(t, a)
			if _, err := a.s.db.Exec(`UPDATE workflow_attempts SET state='failed',run_state=? WHERE id=?`, state, attempt.ID); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"Build", "Fix"} {
				m, err := a.workflow.QueueRunChat(t.Context(), mode, attempt.ID, uuid.NewString(), "continue", "")
				if err != nil || m.Status != "queued" || m.Classification != "continuation" {
					t.Fatal(m, err)
				}
			}
		})
	}
	for _, state := range []string{"running", "failed", "cancelled", "approved"} {
		t.Run("planning-"+state, func(t *testing.T) {
			a := testApp(t)
			r := planningHistoryFixture(t, a)
			if _, err := a.s.db.Exec(`UPDATE planning_turns SET agent_id='planner',run_id='planner-run',remote_attempt_id='remote' WHERE id=?`, r.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := a.s.db.Exec(`UPDATE planning_sessions SET state=? WHERE card_id=?`, state, r.CardID); err != nil {
				t.Fatal(err)
			}
			m, err := a.workflow.QueueRunChat(t.Context(), "Planning", r.ID, uuid.NewString(), "planning chat", "")
			if err != nil {
				t.Fatal(err)
			}
			want := "continuation"
			if state == "cancelled" || state == "approved" {
				want = "followup"
			}
			if m.Classification != want {
				t.Fatal(m)
			}
		})
	}
}

func TestRunChatDowngradeAndPinnedIdentity(t *testing.T) {
	a := testApp(t)
	attempt := pinnedDetailAttempt(t, a)
	m, err := a.workflow.QueueRunChat(t.Context(), "Build", attempt.ID, uuid.NewString(), "historical", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.s.db.Exec(`UPDATE workflow_attempts SET ready=1,state='ready' WHERE id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := a.s.writeTx()
	if err != nil {
		t.Fatal(err)
	}
	r, err := resolveChat(t.Context(), tx, "Build", attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	class, err := classifyChat(tx, r)
	_ = tx.Rollback()
	if err != nil || class != "followup" {
		t.Fatal(class, err)
	}
	if _, err = a.s.db.Exec(`UPDATE workflow_attempts SET run_id='changed-run' WHERE id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	// dispatch before network must visibly reject an identity change.
	a.workflow.dispatchChatMessage(t.Context(), m)
	got := a.workflow.chatMessages(buildRun(attempt))
	if got[0].Status != "failed" || got[0].Failure != "identity_changed" {
		t.Fatal(got)
	}
}

func TestRunChatFollowupFencesWorkflowEvidence(t *testing.T) {
	a := testApp(t)
	attempt := pinnedDetailAttempt(t, a)
	if _, err := a.s.db.Exec(`UPDATE workflow_attempts SET state='failed',run_state='abandoned',error='original fence' WHERE id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	m, err := a.workflow.QueueRunChat(t.Context(), "Build", attempt.ID, uuid.NewString(), "followup", "")
	if err != nil || m.Classification != "followup" {
		t.Fatal(m, err)
	}
	if _, err = a.s.db.Exec(`UPDATE run_chat SET request_json='{}',status='active' WHERE id=?`, m.ID); err != nil {
		t.Fatal(err)
	}
	target := client.Target{TaskID: attempt.TaskID, AgentID: attempt.AgentID, RunID: attempt.RunID, AttemptID: attempt.RemoteAttemptID}
	if err = a.workflow.event(t.Context(), client.StatusEvent{Target: target, Version: 1, EventID: "late", Seq: 999, State: runtime.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if err = a.workflow.result(t.Context(), client.Outcome{Version: 1, TaskID: target.TaskID, AgentID: target.AgentID, RunID: target.RunID, AttemptID: target.AttemptID, State: runtime.RunCompleted}); err != nil {
		t.Fatal(err)
	}
	after, err := a.workflow.attemptByID(attempt.ID)
	if err != nil || after.RunState != "abandoned" || after.Error != "original fence" || after.Ready {
		t.Fatal(after, err)
	}
	req, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: target.TaskID, AgentID: target.AgentID, RunID: target.RunID, AttemptID: target.AttemptID, TurnID: "followup-turn", ToolCallID: "call"}, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 123})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.workflow.acceptReport(context.Background(), req); err == nil {
		raw, _ := json.Marshal(req)
		t.Fatalf("accepted followup report %s", raw)
	}
}

func TestRunChatPlanningProposalSurvivesHostReceipt(t *testing.T) {
	a := testApp(t)
	r := planningHistoryFixture(t, a)
	w := a.workflow
	w.observeReport = fakeReportObserver
	first := reportRequest(t, r.TaskID, reportprotocol.Clarify, reportprotocol.Payload{Questions: []string{"Which scope?"}})
	if ack, err := w.acceptReport(t.Context(), first); err != nil || !ack.Accepted {
		t.Fatal(ack, err)
	}
	old := client.Outcome{Version: 1, TaskID: r.TaskID, AgentID: first.AgentID, RunID: first.RunID, AttemptID: first.AttemptID, State: runtime.RunCompleted, Response: "Stopped"}
	if err := w.result(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	message, err := w.QueueRunChat(t.Context(), "Planning", r.ID, uuid.NewString(), "Please propose", "")
	if err != nil {
		t.Fatal(err)
	}
	cmd := reportprotocol.Chat{Version: 1, ID: message.ID, Target: client.Target{TaskID: r.TaskID, AgentID: first.AgentID, RunID: first.RunID, AttemptID: first.AttemptID}, Text: message.Text}
	raw, _ := json.Marshal(cmd)
	if _, err = a.s.db.Exec(`UPDATE run_chat SET status='dispatched',request_json=? WHERE id=?`, string(raw), message.ID); err != nil {
		t.Fatal(err)
	}
	w.applyChatReceipt(t.Context(), message, cmd, reportprotocol.ChatReceipt{Status: "active"})
	d := planningDetails(t, w, r.CardID)
	if d.Session.State != "running" || d.Session.Error != "" {
		t.Fatalf("not investigating: %+v", d.Session)
	}
	proposal := reportRequest(t, r.TaskID, reportprotocol.ProposeSpec, reportprotocol.Payload{Spec: "Actual proposal", RepoCommit: strings.Repeat("a", 40)})
	proposal, err = reportprotocol.NewRequest(reportprotocol.Identity{TaskID: r.TaskID, AgentID: first.AgentID, RunID: first.RunID, AttemptID: first.AttemptID, TurnID: first.TurnID, ToolCallID: "proposal-call"}, reportprotocol.ProposeSpec, proposal.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := w.acceptReport(t.Context(), proposal); err != nil || !ack.Accepted {
		t.Fatal(ack, err)
	}
	// Even a late natural completion cannot overwrite a committed proposal.
	w.applyChatReceipt(t.Context(), message, cmd, reportprotocol.ChatReceipt{Status: "completed", Outcome: &old})
	waiting := client.Outcome{Version: 1, TaskID: r.TaskID, AgentID: proposal.AgentID, RunID: proposal.RunID, AttemptID: proposal.AttemptID, State: runtime.RunWaiting, AnswerSubject: "bonnie.answers." + proposal.AgentID, Suspend: &runtime.SuspendRequest{TurnID: proposal.TurnID, ToolCallID: proposal.ToolCallID, Kind: "question"}}
	w.applyChatReceipt(t.Context(), message, cmd, reportprotocol.ChatReceipt{Status: "completed", Outcome: &waiting})
	// Redelivery from the original task must not erase the resumed suspension.
	if err = w.result(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	d = planningDetails(t, w, r.CardID)
	if d.Session.State != "ready" || !d.CanApprove || d.Session.Error != "" || d.InteractionToken != proposal.ReportID {
		t.Fatalf("proposal lost: %+v", d)
	}
	var saved string
	if err = a.s.db.QueryRow(`SELECT waiting_json FROM planning_interactions WHERE report_id=?`, proposal.ReportID).Scan(&saved); err != nil || saved == "" {
		t.Fatal("suspension missing", err)
	}
	if err = w.ReplyPlanning(r.CardID, "Refine it", proposal.ReportID); err != nil {
		t.Fatal(err)
	}
}

func TestRunChatExactInteractionOutboxes(t *testing.T) {
	for _, mode := range []string{"Build", "Planning"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			var record RunRecord
			if mode == "Build" {
				record = buildRun(pinnedDetailAttempt(t, a))
			} else {
				record = planningHistoryFixture(t, a)
				if _, err := a.s.db.Exec(`UPDATE planning_turns SET agent_id='planner',run_id='planner-run',remote_attempt_id='remote' WHERE id=?`, record.ID); err != nil {
					t.Fatal(err)
				}
				record, _ = a.workflow.historyByID(t.Context(), mode, record.ID)
			}
			kind := reportprotocol.ReportBlocker
			payload := reportprotocol.Payload{Reason: "blocked"}
			if mode == "Planning" {
				kind = reportprotocol.ProposeSpec
				payload = reportprotocol.Payload{Spec: "plan", RepoCommit: strings.Repeat("a", 40)}
			}
			req, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: record.TaskID, AgentID: record.AgentID, RunID: record.RunID, AttemptID: record.RemoteAttemptID, TurnID: "wait-turn", ToolCallID: "wait-call"}, kind, payload)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(req)
			if _, err = a.s.db.Exec(`INSERT INTO typed_reports(report_id,task_id,tool_call_id,agent_id,run_id,attempt_id,turn_id,kind,request_json,accepted_at) VALUES(?,?,?,?,?,?,?,?,?,1)`, req.ReportID, req.TaskID, req.ToolCallID, req.AgentID, req.RunID, req.AttemptID, req.TurnID, kind, string(raw)); err != nil {
				t.Fatal(err)
			}
			table := "workflow_interactions"
			if mode == "Planning" {
				table = "planning_interactions"
			}
			if _, err = a.s.db.Exec(`INSERT INTO `+table+`(report_id,task_id,created_at) VALUES(?,?,1)`, req.ReportID, req.TaskID); err != nil {
				t.Fatal(err)
			}
			stale, err := a.workflow.QueueRunChat(t.Context(), mode, record.ID, uuid.NewString(), "stale guidance", "old-token")
			if err != nil {
				t.Fatal(err)
			}
			a.workflow.dispatchChatMessage(t.Context(), stale)
			m, err := a.workflow.QueueRunChat(t.Context(), mode, record.ID, uuid.NewString(), "revise without approving", req.ReportID)
			if err != nil {
				t.Fatal(err)
			}
			a.workflow.dispatchChatMessage(t.Context(), m)
			var state, text string
			if err = a.s.db.QueryRow(`SELECT status,reply FROM `+table+` WHERE report_id=?`, req.ReportID).Scan(&state, &text); err != nil || state != "queued" || text != m.Text {
				t.Fatal(state, text, err)
			}
			if mode == "Planning" {
				var approved bool
				if err = a.s.db.QueryRow(`SELECT approved FROM planning_interactions WHERE report_id=?`, req.ReportID).Scan(&approved); err != nil || approved {
					t.Fatal(approved, err)
				}
			}
			if _, err = a.s.db.Exec(`UPDATE `+table+` SET status='answered' WHERE report_id=?`, req.ReportID); err != nil {
				t.Fatal(err)
			}
			a.workflow.reconcileChatInteractions()
			got := a.workflow.chatMessages(record)
			if got[0].Status != "failed" || got[1].Status != "completed" {
				t.Fatal(got)
			}
		})
	}
}
