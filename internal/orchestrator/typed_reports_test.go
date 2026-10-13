package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
)

func reportRequest(t *testing.T, task string, kind reportprotocol.Kind, p reportprotocol.Payload) reportprotocol.Request {
	t.Helper()
	r, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: task, AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "turn", ToolCallID: "call"}, kind, p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTypedReportCommitReplayConflictAndNaturalOutcome(t *testing.T) {
	s, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(c.ID)
	r := reportRequest(t, a.TaskID, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 7})
	w.observeReport = fakeReportObserver
	// A failing inbox insert rolls back the workflow transition too.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_report BEFORE INSERT ON typed_reports BEGIN SELECT RAISE(ABORT,'receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if ack, err := w.acceptReport(t.Context(), r); err == nil || ack.Accepted {
		t.Fatal("ack before durable commit", ack, err)
	}
	a, _ = w.CardResult(c.ID)
	if a.PRURL != "" || a.OutcomeJSON != "" {
		t.Fatal("partial transition", a)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_report`); err != nil {
		t.Fatal(err)
	}
	// Concurrent exact retries produce one transition and one duplicate receipt.
	var wg sync.WaitGroup
	receipts := make(chan reportprotocol.Ack, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { ack, err := w.acceptReport(t.Context(), r); receipts <- ack; errs <- err })
	}
	wg.Wait()
	close(receipts)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	duplicates := 0
	for ack := range receipts {
		if !ack.Accepted {
			t.Fatal(ack)
		}
		if ack.Duplicate {
			duplicates++
		}
	}
	if duplicates != 1 {
		t.Fatal("duplicate count", duplicates)
	}
	a, _ = w.CardResult(c.ID)
	if a.PRURL != "https://github.com/example/repo/pull/7" || a.Ready || a.Error != "awaiting verification" {
		t.Fatal(a)
	}
	// Lost ACK replay after restart/recovery turn never requires the current turn.
	reopened := &Workflow{s: s, ctx: context.Background(), observeReport: func(context.Context, client.Target) (client.Status, error) {
		t.Error("duplicate queried runtime")
		return client.Status{}, errors.New("offline")
	}}
	ack, err := reopened.acceptReport(t.Context(), r)
	if err != nil || !ack.Accepted || !ack.Duplicate {
		t.Fatal(ack, err)
	}
	for _, field := range []string{"agent", "run", "attempt", "turn"} {
		spoof := r
		switch field {
		case "agent":
			spoof.AgentID = "evil"
		case "run":
			spoof.RunID = "evil"
		case "attempt":
			spoof.AttemptID = "evil"
		case "turn":
			spoof.TurnID = "evil"
		}
		if ack, err := w.acceptReport(t.Context(), spoof); err == nil || ack.Accepted {
			t.Fatal("duplicate owner bypass", field, ack, err)
		}
	}
	conflict := reportRequest(t, a.TaskID, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "different terminal report"})
	if _, err := w.acceptReport(t.Context(), conflict); err == nil {
		t.Fatal("conflict accepted")
	}
	for _, text := range []string{`{"pr_number":99}`, "{broken", "done"} {
		if err := w.result(t.Context(), client.Outcome{TaskID: a.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID, State: runtime.RunCompleted, Response: text}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.event(t.Context(), client.StatusEvent{TaskID: a.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID, EventID: "late", Seq: 999, State: runtime.RunFailed}); err != nil {
		t.Fatal(err)
	}
	current, _ := w.CardResult(c.ID)
	if current.OutcomeJSON != a.OutcomeJSON || current.Error != a.Error || current.PRURL != a.PRURL {
		t.Fatal("natural delivery overrode receipt", current)
	}
	w.SetVerifier(func(_ context.Context, observed Attempt) (bool, error) { return observed.PRURL == a.PRURL, nil })
	w.verifyPending()
	current, _ = w.CardResult(c.ID)
	if !current.Ready {
		t.Fatal("existing verification contract lost", current)
	}
	if ack, err := reopened.acceptReport(t.Context(), r); err != nil || !ack.Duplicate {
		t.Fatal(ack, err)
	}
}

func TestTypedReportExactRuntimeAuthorizationAndMode(t *testing.T) {
	for _, field := range []string{"task", "agent", "run", "attempt", "turn", "inactive", "cancelled", "unavailable", "mode", "hash"} {
		t.Run(field, func(t *testing.T) {
			_, w, c := planningLocal(t)
			if err := w.MoveCard(c.ID, "Building", 0); err != nil {
				t.Fatal(err)
			}
			a, _ := w.CardResult(c.ID)
			r := reportRequest(t, a.TaskID, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 1})
			w.observeReport = func(ctx context.Context, target client.Target) (client.Status, error) {
				status, _ := fakeReportObserver(ctx, target)
				switch field {
				case "task":
					status.TaskID = "wrong"
				case "agent":
					status.AgentID = "wrong"
				case "run":
					status.RunID = "wrong"
				case "attempt":
					status.AttemptID = "wrong"
				case "turn":
					status.TurnID = "wrong"
				case "inactive":
					status.Active = false
				case "cancelled":
					status.State = runtime.RunCancelled
				case "unavailable":
					return status, errors.New("offline")
				}
				return status, nil
			}
			if field == "mode" {
				r = reportRequest(t, a.TaskID, reportprotocol.ProposeSpec, reportprotocol.Payload{Spec: "plan", RepoCommit: strings.Repeat("a", 40)})
			}
			if field == "hash" {
				r.ReportID = "bad"
			}
			if ack, err := w.acceptReport(t.Context(), r); err == nil || ack.Accepted {
				t.Fatal("invalid authority accepted", ack, err)
			}
			current, _ := w.CardResult(c.ID)
			if current.OutcomeJSON != "" || current.PRURL != "" {
				t.Fatal("rejection changed task", current)
			}
		})
	}
}

func TestTypedPlanningProposalQuestionsReceiptAndBlocker(t *testing.T) {
	_, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	d := planningDetails(t, w, c.ID)
	r := reportRequest(t, d.Turns[0].ID, reportprotocol.Clarify, reportprotocol.Payload{Questions: []string{"Which API?"}})
	w.observeReport = fakeReportObserver
	ack, err := w.acceptReport(t.Context(), r)
	if err != nil || !ack.Accepted {
		t.Fatal(ack, err)
	}
	if ack, err = w.acceptReport(t.Context(), r); err != nil || !ack.Duplicate {
		t.Fatal(ack, err)
	}
	d = planningDetails(t, w, c.ID)
	if d.CanApprove || len(d.Revisions) != 0 || len(d.Messages) != 2 || len(d.Questions) != 1 {
		t.Fatal(d)
	}
	if err = w.ReplyPlanning(c.ID, "API v2", d.InteractionToken); err != nil {
		t.Fatal(err)
	}
	if _, err = w.s.db.Exec(`UPDATE planning_interactions SET status='answered' WHERE report_id=?`, r.ReportID); err != nil {
		t.Fatal(err)
	}
	next, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.AttemptID, TurnID: r.TurnID, ToolCallID: "proposal"}, reportprotocol.ProposeSpec, reportprotocol.Payload{Spec: "## Revised plan", RepoCommit: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	if ack, err = w.acceptReport(t.Context(), next); err != nil || !ack.Accepted {
		t.Fatal(ack, err)
	}
	d = planningDetails(t, w, c.ID)
	if !d.CanApprove || d.Session.Revision != 1 || len(d.Revisions) != 1 {
		t.Fatal(d)
	}
	if err = w.ApprovePlanning(c.ID, 2); err == nil {
		t.Fatal("stale approval accepted")
	}
	if err = w.ApprovePlanning(c.ID, 1); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(c.ID)
	blocker := reportRequest(t, a.TaskID, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "Missing repository access"})
	if ack, err = w.acceptReport(t.Context(), blocker); err != nil || !ack.Accepted || !ack.AwaitGuidance {
		t.Fatal(ack, err)
	}
	// The blocker parks the run for guidance; it is not a terminal failure.
	a, _ = w.CardResult(c.ID)
	if a.State != "blocked" || a.Error != blocker.Payload.Reason || a.PRURL != "" || terminalFailure(a) {
		t.Fatal(a)
	}
	if err = w.Retry(c.ID); err == nil {
		t.Fatal("fresh retry replaced a run awaiting guidance")
	}
	// Ending the parked run (for example by cancellation) restores fresh Retry.
	if err = w.result(t.Context(), client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", State: runtime.RunCancelled}); err != nil {
		t.Fatal(err)
	}
	if a, _ = w.CardResult(c.ID); !terminalFailure(a) {
		t.Fatal(a)
	}
	if err = w.Retry(c.ID); err != nil {
		t.Fatal(err)
	}
	if ack, err = w.acceptReport(t.Context(), blocker); err != nil || !ack.Duplicate || !ack.AwaitGuidance {
		t.Fatal("original receipt replay lost on retry", ack, err)
	}
}

func TestNaturalPlanningJSONNeverProposes(t *testing.T) {
	_, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	out := planningOutcome(t, w, c.ID, "Looks valid", "Question?")
	if err := w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	d := planningDetails(t, w, c.ID)
	if d.Session.State != "failed" || len(d.Revisions) != 0 || !strings.Contains(d.Session.Error, "without an accepted typed report") {
		t.Fatal(d)
	}
}

func TestTypedPlanningBlockerAndPayloadRejections(t *testing.T) {
	_, w, c := planningLocal(t)
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	d := planningDetails(t, w, c.ID)
	w.observeReport = fakeReportObserver
	r := reportRequest(t, d.Turns[0].ID, reportprotocol.ProposeSpec, reportprotocol.Payload{Spec: "plan", RepoCommit: strings.Repeat("a", 40)})
	for _, p := range []reportprotocol.Payload{
		{Spec: "", RepoCommit: strings.Repeat("a", 40)},
		{Spec: "plan", RepoCommit: "abc"},
		{Spec: "plan", RepoCommit: strings.Repeat("a", 40), Questions: []string{""}},
		{Spec: "plan", RepoCommit: strings.Repeat("a", 40), Questions: []string{strings.Repeat("x", 4001)}},
		{Spec: "plan", RepoCommit: strings.Repeat("a", 40), Questions: make([]string, 31)},
	} {
		invalid := r
		invalid.Payload = p
		if ack, err := w.acceptReport(t.Context(), invalid); err == nil || ack.Accepted {
			t.Fatal("invalid payload accepted", ack, err)
		}
	}
	complete := reportRequest(t, r.TaskID, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 1})
	if _, err := w.acceptReport(t.Context(), complete); err == nil {
		t.Fatal("build report accepted in planning")
	}
	blocker := reportRequest(t, r.TaskID, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "Cannot inspect repository"})
	if ack, err := w.acceptReport(t.Context(), blocker); err != nil || !ack.Accepted || !ack.AwaitGuidance {
		t.Fatal(ack, err)
	}
	// The planner waits for guidance in the same run instead of failing.
	d = planningDetails(t, w, c.ID)
	if d.Session.State != "questions" || d.Session.Error != blocker.Payload.Reason || d.Blocker != blocker.Payload.Reason || d.InteractionToken != blocker.ReportID || len(d.Revisions) != 0 || len(d.Messages) != 2 || len(d.Discussion) != 1 {
		t.Fatal(d)
	}
	if err := w.RetryPlanning(c.ID); err == nil {
		t.Fatal("fresh retry replaced a planner awaiting guidance")
	}
	if err := w.ReplyPlanning(c.ID, "Use the public mirror", blocker.ReportID); err != nil {
		t.Fatal(err)
	}
	var status, reply string
	if err := w.s.db.QueryRow(`SELECT status,reply FROM planning_interactions WHERE report_id=?`, blocker.ReportID).Scan(&status, &reply); err != nil || status != "queued" || reply != "Use the public mirror" {
		t.Fatal(status, reply, err)
	}
	if d = planningDetails(t, w, c.ID); d.Session.State != "running" || d.Blocker != "" {
		t.Fatal(d)
	}
	if _, err := w.acceptReport(t.Context(), r); err == nil {
		t.Fatal("old conflicting proposal accepted after guidance")
	}
	if ack, err := w.acceptReport(t.Context(), blocker); err != nil || !ack.Duplicate {
		t.Fatal("original blocker receipt unavailable", ack, err)
	}
}
