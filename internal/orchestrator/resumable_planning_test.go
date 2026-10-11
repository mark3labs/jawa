package orchestrator

import (
	"context"
	"encoding/json"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
	"jawa/internal/reportprotocol"
)

func TestPlanningReplyBeforeSuspendExactMatchAndDisconnectedOutbox(t *testing.T) {
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	w := &Workflow{s: s, ctx: context.Background(), observeReport: fakeReportObserver}
	cli, err := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true})
	if err != nil {
		t.Fatal(err)
	}
	w.client = cli
	if err = w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	d := planningDetails(t, w, c.ID)
	r := reportRequest(t, d.Turns[0].ID, reportprotocol.Clarify, reportprotocol.Payload{Questions: []string{"Which API?"}})
	if _, err = w.acceptReport(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err = w.ReplyPlanning(c.ID, "API v2", r.ReportID); err != nil {
		t.Fatal(err)
	}
	if err = w.ReplyPlanning(c.ID, "changed", r.ReportID); err == nil {
		t.Fatal("double reply accepted")
	}
	answers, err := nc.SubscribeSync("bonnie.answers.agent")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = answers.Unsubscribe() }()
	_ = nc.Flush()
	w.dispatchPlanningReplies()
	var status, raw string
	if err = s.db.QueryRow(`SELECT status,waiting_json FROM planning_interactions WHERE report_id=?`, r.ReportID).Scan(&status, &raw); err != nil || status != "queued" || raw != "" {
		t.Fatal(status, raw, err)
	}
	out := client.Outcome{Version: 1, TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID, State: runtime.RunWaiting, AnswerSubject: "bonnie.answers.agent", Suspend: &runtime.SuspendRequest{TurnID: r.TurnID, ToolCallID: r.ToolCallID, Kind: "question"}}
	for _, field := range []string{"call", "turn", "run", "agent", "attempt", "route"} {
		bad := out
		suspension := *out.Suspend
		bad.Suspend = &suspension
		switch field {
		case "call":
			bad.Suspend.ToolCallID = "wrong"
		case "turn":
			bad.Suspend.TurnID = "wrong"
		case "run":
			bad.RunID = "wrong"
		case "agent":
			bad.AgentID = "wrong"
		case "attempt":
			bad.AttemptID = "wrong"
		case "route":
			bad.AnswerSubject = "bonnie.answers.other"
		}
		if err = w.result(t.Context(), bad); err != nil {
			t.Fatal(err)
		}
		_ = s.db.QueryRow(`SELECT waiting_json FROM planning_interactions WHERE report_id=?`, r.ReportID).Scan(&raw)
		if raw != "" {
			t.Fatal("mismatched waiting outcome admitted", field)
		}
	}
	if err = w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	// A closed transport must not discard the persisted reply. Restart uses the
	// same report, task, run, tool-call and immutable text with no new task.
	offline, err := nats.Connect(nc.ConnectedUrl())
	if err != nil {
		t.Fatal(err)
	}
	offlineClient, err := client.New(offline, client.Config{RootSubject: "bonnie"})
	if err != nil {
		t.Fatal(err)
	}
	offline.Close()
	w.client = offlineClient
	w.dispatchPlanningReplies()
	_ = s.db.QueryRow(`SELECT status FROM planning_interactions WHERE report_id=?`, r.ReportID).Scan(&status)
	if status != "queued" {
		t.Fatal("offline reply lost", status)
	}
	restarted := &Workflow{s: s, ctx: context.Background(), client: cli}
	restarted.dispatchPlanningReplies()
	msg, err := answers.NextMsgWithContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var answer client.Answer
	if err = json.Unmarshal(msg.Data, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.TaskID != r.TaskID || answer.RunID != r.RunID || answer.ToolCallID != r.ToolCallID || len(answer.Responses) != 1 || answer.Responses[0].TurnID != r.TurnID || answer.Responses[0].Text != "API v2" {
		t.Fatal(answer)
	}
	d = planningDetails(t, restarted, c.ID)
	if len(d.Turns) != 1 || d.Session.State != "running" {
		t.Fatal(d)
	}
	if ack, err := restarted.acceptReport(t.Context(), r); err != nil || !ack.Duplicate {
		t.Fatal(ack, err)
	}
}

func TestPlanningUnexpectedCompletionWithProposalFailsAndCancellationRevokesOutbox(t *testing.T) {
	s, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	d := planningDetails(t, w, c.ID)
	r := reportRequest(t, d.Turns[0].ID, reportprotocol.Clarify, reportprotocol.Payload{Questions: []string{"Why?"}})
	w.observeReport = fakeReportObserver
	if _, err := w.acceptReport(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	out := client.Outcome{TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID, State: runtime.RunCompleted}
	if err := w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	d = planningDetails(t, w, c.ID)
	if d.Session.State != "failed" || d.InteractionToken != "" {
		t.Fatal(d)
	}
	if err := w.RetryPlanning(c.ID); err != nil {
		t.Fatal(err)
	}
	d = planningDetails(t, w, c.ID)
	r = reportRequest(t, d.Turns[1].ID, reportprotocol.Clarify, reportprotocol.Payload{Questions: []string{"Why?"}})
	if _, err := w.acceptReport(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := w.ReplyPlanning(c.ID, "because", r.ReportID); err != nil {
		t.Fatal(err)
	}
	if err := w.CancelPlanning(c.ID); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM planning_interactions WHERE report_id=?`, r.ReportID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatal(status, err)
	}
	if err := w.ReplyPlanning(c.ID, "late", r.ReportID); err == nil {
		t.Fatal("cancelled reply accepted")
	}
}
