package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"jawa/internal/reportprotocol"
)

func TestWorkflowDiscussionHistory(t *testing.T) {
	t.Parallel()
	w, c, r := parkedBuild(t)
	if _, err := w.acceptReport(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := w.GuideBlocked(c.ID, "Saved **guidance**", r.ReportID); err != nil {
		t.Fatal(err)
	}
	if err := w.GuideBlocked(c.ID, "duplicate", r.ReportID); err == nil {
		t.Fatal("duplicate accepted")
	}
	// Exercise readback of every durable status, including cancellation after queueing.
	for _, status := range []string{"queued", "answered", "cancelled"} {
		if _, err := w.s.db.Exec(`UPDATE workflow_interactions SET status=? WHERE report_id=?`, status, r.ReportID); err != nil {
			t.Fatal(err)
		}
		history, err := w.s.workflowDiscussion(c.ID)
		if err != nil || len(history) != 1 || history[0].Reply != "Saved **guidance**" || history[0].Status != status {
			t.Fatal(history, err)
		}
	}
	// Insert historical accepted evidence with equal acceptance times: rowid must
	// preserve distinct reports with identical reasons. No interaction = legacy.
	for i, kind := range []reportprotocol.Kind{reportprotocol.ReportBlocker, reportprotocol.ReportBlocker, reportprotocol.CompleteTask} {
		rr := r
		rr.ReportID = fmt.Sprintf("history-%d", i)
		rr.ToolCallID = rr.ReportID
		rr.Kind = kind
		raw, err := json.Marshal(rr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.s.db.Exec(`INSERT INTO typed_reports(task_id,report_id,tool_call_id,agent_id,run_id,attempt_id,turn_id,kind,request_json,accepted_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, rr.TaskID, rr.ReportID, rr.ToolCallID, rr.AgentID, rr.RunID, rr.AttemptID, rr.TurnID, rr.Kind, string(raw), 9223372036854775800); err != nil {
			t.Fatal(err)
		}
	}
	history, err := w.s.workflowDiscussion(c.ID)
	if err != nil || len(history) != 3 || history[1].ReportID != "history-0" || history[2].ReportID != "history-1" || history[2].Reply != "" {
		t.Fatal(history, err)
	}
	// Retry adds an attempt, not a new message for an operational error.
	if _, err = w.s.db.Exec(`UPDATE workflow_attempts SET state='failed',run_state='failed',error='provider unavailable' WHERE task_id=?`, r.TaskID); err != nil {
		t.Fatal(err)
	}
	if err = w.Retry(c.ID); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(c.ID)
	next := reportRequest(t, a.TaskID, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: r.Payload.Reason})
	if _, err = w.acceptReport(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	history, err = w.s.workflowDiscussion(c.ID)
	if err != nil || len(history) != 4 || history[3].Number != 2 || history[3].ReportID != next.ReportID {
		t.Fatal(history, err)
	}
	if other, err := w.s.workflowDiscussion("other-card"); err != nil || len(other) != 0 {
		t.Fatal(other, err)
	}
	if _, err = w.s.db.Exec(`UPDATE typed_reports SET request_json='{' WHERE report_id=?`, next.ReportID); err != nil {
		t.Fatal(err)
	}
	if _, err = w.s.workflowDiscussion(c.ID); err == nil {
		t.Fatal("decode error hidden")
	}
}

func TestGuidanceCancellationPresentation(t *testing.T) {
	t.Parallel()
	w, c, r := parkedBuild(t)
	for _, status := range []string{"pending", "requested"} {
		if _, err := w.s.db.Exec(`INSERT INTO workflow_cancel_requests(task_id,status,created_at) VALUES(?,?,0)`, r.TaskID, status); err != nil {
			t.Fatal(err)
		}
		blockers, err := w.s.blockerGuidance()
		if err != nil || !blockers[c.ID].Cancelling {
			t.Fatal(blockers, err)
		}
		a, _ := w.CardResult(c.ID)
		d := pageData{blockers: blockers, attempts: []Attempt{a}}
		c.Status = "Building"
		if d.canGuide(c) {
			t.Fatal("cancellation still invites guidance")
		}
		if err := w.GuideBlocked(c.ID, "Too late", r.ReportID); err == nil {
			t.Fatal("cancellation accepted guidance")
		}
	}
}

func TestFixWorkflowDiscussion(t *testing.T) {
	t.Parallel()
	w, c, r := parkedBuild(t)
	if _, err := w.s.db.Exec(`UPDATE cards SET status='Fix' WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.GuideBlocked(c.ID, "Fix guidance", r.ReportID); err != nil {
		t.Fatal(err)
	}
	d, err := loadPageData(t.Context(), w.s, nil, "csrf", viewParams{View: viewPlanning, Card: c.ID})
	if err != nil || len(d.workflowDiscussion) != 1 || d.workflowDiscussion[0].Reply != "Fix guidance" {
		t.Fatal(d.workflowDiscussion, err)
	}
	board, err := loadPageData(t.Context(), w.s, nil, "csrf", viewParams{View: viewBoard})
	if err != nil || len(board.workflowDiscussion) != 0 {
		t.Fatal(board.workflowDiscussion, err)
	}

}

func guidanceFixtureData(lane string) pageData {
	c := Card{ID: "card", Title: "Blocked card", Description: "Original request", Status: lane}
	return pageData{csrf: "test-csrf", v: viewParams{View: viewPlanning, Card: c.ID},
		cards: []Card{c}, natsConfigured: true,
		attempts: []Attempt{{ID: "attempt", CardID: c.ID, TaskID: "task", Number: 1, State: "blocked", RunState: "waiting"}},
		blockers: map[string]BlockerGuidance{c.ID: {CardID: c.ID, TaskID: "task", Token: "report-token", Reason: "Need **help**"}}, workflowDiscussion: []WorkflowExchange{{CardID: c.ID, TaskID: "task", AttemptID: "attempt", Number: 1, ReportID: "report-token", Reason: "Need **help**\n\n<script>alert(1)</script>\n\n" + strings.Repeat("long-text", 100)}}}
}

func TestWorkflowDiscussionFullCard(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Building", "Fix"} {
		for _, state := range []string{"pending", "queued", "answered", "cancelled", "cancelling", "ready", "running", "legacy"} {
			t.Run(lane+state, func(t *testing.T) {
				d := guidanceFixtureData(lane)
				b := d.blockers["card"]
				switch state {
				case "queued":
					b.Queued = true
					d.workflowDiscussion[0].Reply = "Persisted **reply**"
				case "answered", "cancelled", "legacy":
					delete(d.blockers, "card")
				case "cancelling":
					b.Cancelling = true
				case "running":
					d.attempts[0].State = "running"
				case "ready":
					d.attempts[0].Ready = true
				}
				if _, ok := d.blockers["card"]; ok {
					d.blockers["card"] = b
				}
				var out bytes.Buffer
				if err := planningContent(d).Render(t.Context(), &out); err != nil {
					t.Fatal(err)
				}
				html := out.String()
				if strings.Contains(html, `action="/cards/guide"`) != (state == "pending") {
					t.Fatal(html)
				}
				for _, want := range []string{`aria-label="Discussion"`, `id="workflow-message-report-token-assistant"`, `<strong>help</strong>`} {
					if !strings.Contains(html, want) {
						t.Fatal(want, html)
					}
				}
				if strings.Contains(html, "<script>") {
					t.Fatal("unsafe Markdown", html)
				}
				if state == "pending" {
					for _, want := range []string{`id="guidance-reply-form-report-token"`, `name="expected_interaction" value="report-token"`, `name="message"`, `data-ignore-morph`, `data-reset-on-success`, `maxlength="64000"`, `name="_csrf"`, `name="card_id"`, `name="card"`} {
						if !strings.Contains(html, want) {
							t.Fatal(want, html)
						}
					}
				}
				if state == "queued" && (!strings.Contains(html, "Persisted <strong>reply</strong>") || !strings.Contains(html, guidanceQueuedNotice)) {
					t.Fatal(html)
				}
			})
		}
	}
	// Planning discussion precedes workflow history, without a planning composer
	// after approval/build handoff.
	d := guidanceFixtureData("Building")
	d.planning = map[string]PlanningDetail{"card": {Discussion: []PlanningMessage{{Role: "user", Content: "Planning feedback"}}}}
	var out bytes.Buffer
	if err := planningContent(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Index(out.String(), "Planning feedback") > strings.Index(out.String(), "workflow-message-") || strings.Contains(out.String(), `action="/cards/planning/reply"`) {
		t.Fatal(out.String())
	}
}

// Export production page and patch roots for the embedded-bundle browser test.
func TestGuidanceBrowserFixture(t *testing.T) {
	path := os.Getenv("JAWA_GUIDANCE_HTML")
	if path == "" {
		t.Skip("set JAWA_GUIDANCE_HTML to export fixture")
	}
	d := guidanceFixtureData("Building")
	for _, state := range []string{"page", "pending", "queued", "next"} {
		var out bytes.Buffer
		switch state {
		case "queued":
			b := d.blockers["card"]
			b.Queued = true
			d.blockers["card"] = b
			d.workflowDiscussion[0].Reply = "Saved guidance"
		case "next":
			b := d.blockers["card"]
			b.Queued = false
			b.Token = "next-token"
			d.blockers["card"] = b
			d.workflowDiscussion = append(d.workflowDiscussion, WorkflowExchange{Number: 1, ReportID: "next-token", Reason: "Another blocker"})
		}
		if state == "page" {
			if err := appPage(d).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := planningContent(d).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
		}
		suffix := ""
		if state != "page" {
			suffix = "." + state
		}
		if err := os.WriteFile(path+suffix, out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
