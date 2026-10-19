package orchestrator

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"jawa/internal/reportprotocol"
)

func TestPlanningDiscussionFallbackRunChat(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	_, csrf := setupTest(t, a)
	r := planningHistoryFixture(t, a)
	if _, err := a.s.db.Exec(`UPDATE planning_turns SET agent_id='planner',run_id='planner-run',remote_attempt_id='remote' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	c := Card{ID: r.CardID, Status: "Planning", Title: "Discuss without typed reply"}
	detail := PlanningDetail{Session: PlanningSession{State: "running"}, Discussion: []PlanningMessage{{Role: "assistant", Content: "Current discussion"}}}
	data := pageData{csrf: csrf.Value, v: viewParams{View: viewPlanning, Card: c.ID}, cards: []Card{c}, planning: map[string]PlanningDetail{c.ID: detail}, planningChat: &planningChatDetail{Record: r, MessageID: "message-id", Messages: []RunChatMessage{{ID: "queued-id", Text: "Waiting to deliver", Status: "queued", Classification: "continuation"}}}}
	var out bytes.Buffer
	if err := planningContent(data).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`action="/runs/planning/` + r.ID + `/chat"`, `name="message_id"`, `value="message-id"`, `name="interaction"`, `placeholder="Message the planning run"`, "Waiting to deliver", `class="cn-button`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, `action="/cards/planning/reply"`) {
		t.Fatal("typed reply shown without an interaction")
	}
	data.planning[c.ID] = PlanningDetail{Session: PlanningSession{State: "questions"}, InteractionToken: "typed-token", Questions: []string{"Question?"}}
	out.Reset()
	if err := planningContent(data).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `action="/cards/planning/reply"`) || strings.Contains(out.String(), `id="planning-run-chat-`+r.ID+`"`) {
		t.Fatal("fallback competed with authoritative typed reply")
	}
}

func TestPlanningDiscussionOnlyQuestionsAndReplies(t *testing.T) {
	t.Parallel()
	s, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	w.observeReport = fakeReportObserver
	d := planningDetails(t, w, c.ID)
	if len(d.Discussion) != 0 {
		t.Fatal("original description appeared in discussion")
	}
	task := d.Turns[0].ID
	call := 0
	accept := func(kind reportprotocol.Kind, payload reportprotocol.Payload) string {
		t.Helper()
		// Simulate confirmed delivery of the previous resumable interaction.
		if _, err := s.db.Exec(`UPDATE planning_interactions SET status='answered' WHERE task_id=? AND status='queued'`, task); err != nil {
			t.Fatal(err)
		}
		call++
		r, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: task, AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "turn", ToolCallID: fmt.Sprintf("call-%d", call)}, kind, payload)
		if err != nil {
			t.Fatal(err)
		}
		ack, err := w.acceptReport(t.Context(), r)
		if err != nil || !ack.Accepted {
			t.Fatalf("accept report: %+v %v", ack, err)
		}
		return r.ReportID
	}
	question := "Which API?"
	token := accept(reportprotocol.Clarify, reportprotocol.Payload{Questions: []string{question, "Which branch?"}})
	if err := w.ReplyPlanning(c.ID, "API v2 on main", token); err != nil {
		t.Fatal(err)
	}
	// A proposal can have exactly the same Markdown as a question. It still
	// belongs only in the plan, not the discussion.
	token = accept(reportprotocol.ProposeSpec, reportprotocol.Payload{Spec: question + "\nWhich branch?\n", RepoCommit: strings.Repeat("a", 40)})
	if err := w.ReplyPlanning(c.ID, "Please revise the plan", token); err != nil {
		t.Fatal(err)
	}
	accept(reportprotocol.Clarify, reportprotocol.Payload{Questions: []string{question, "Which branch?"}})
	d = planningDetails(t, w, c.ID)
	want := []string{question + "\nWhich branch?\n", "API v2 on main", "Please revise the plan", question + "\nWhich branch?\n"}
	if len(d.Discussion) != len(want) || len(d.Messages) != 6 || len(d.Revisions) != 1 {
		t.Fatalf("discussion/history: %+v", d)
	}
	for i, content := range want {
		if d.Discussion[i].Content != content {
			t.Fatalf("message %d: %+v", i, d.Discussion[i])
		}
	}
	// Run navigation and the authoritative reply composer must coexist after
	// combining mixed run history with the shared full-card discussion.
	runs, err := s.runHistory(t.Context(), nil)
	if err != nil || len(runs) != 1 {
		t.Fatalf("planning history: %v %v", runs, err)
	}
	c.Status = "Planning"
	var out bytes.Buffer
	if err := planningContent(pageData{v: viewParams{View: viewPlanning, Card: c.ID}, cards: []Card{c}, planning: map[string]PlanningDetail{c.ID: d}, runs: runs}).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `aria-label="Execution history"`) ||
		!strings.Contains(out.String(), `href="`+runs[0].path("chat")+`"`) ||
		strings.Count(out.String(), `action="/cards/planning/reply"`) != 1 {
		t.Fatal("planning history link and single discussion reply form must coexist")
	}
	discussion, _, _ := strings.Cut(strings.Split(out.String(), `aria-label="Discussion"`)[1], "</section>")
	if strings.Count(discussion, `class="plan-message"`) != len(want) || strings.Contains(discussion, c.Title+"\n"+c.Description) {
		t.Fatalf("incorrect rendered discussion: %s", discussion)
	}
}
