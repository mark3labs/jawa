package orchestrator

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"jawa/internal/reportprotocol"
)

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
	var out bytes.Buffer
	if err := planningContent(pageData{v: viewParams{View: viewPlanning, Card: c.ID}, cards: []Card{c}, planning: map[string]PlanningDetail{c.ID: d}}).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	discussion, _, _ := strings.Cut(strings.Split(out.String(), `aria-label="Discussion"`)[1], "</section>")
	if strings.Count(discussion, `class="plan-message"`) != len(want) || strings.Contains(discussion, c.Title+"\n"+c.Description) {
		t.Fatalf("incorrect rendered discussion: %s", discussion)
	}
}
