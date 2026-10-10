package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
	"testing"
)

func fakeReportObserver(ctx context.Context, target client.Target) (client.Status, error) {
	return client.Status{Version: 1, Target: target, TurnID: "turn", Active: true, State: runtime.RunRunning}, nil
}

// Fixtures explicitly submit typed payloads; arbitrary natural outcomes still
// exercise the production non-authoritative result path.
func planningDelivery(t *testing.T, w *Workflow, out client.Outcome) error {
	t.Helper()
	var p reportprotocol.Payload
	if out.State != runtime.RunCompleted || json.Unmarshal([]byte(out.Response), &p) != nil || p.Spec == "" {
		return w.result(t.Context(), out)
	}
	kind := reportprotocol.ProposeSpec
	if len(p.Questions) > 0 {
		kind = reportprotocol.Clarify
		p = reportprotocol.Payload{Questions: p.Questions}
	}
	// Legacy backend fixtures now deliver explicit typed interactions, not terminal proposals.
	call := out.Response
	r, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: out.TaskID, AgentID: out.AgentID, RunID: out.RunID, AttemptID: out.AttemptID, TurnID: "turn", ToolCallID: fmt.Sprintf("call-%x", sha256.Sum256([]byte(call)))}, kind, p)
	if err != nil {
		return err
	}
	// Emulate a stored Answer being consumed before the next scripted report.
	if _, err = w.s.db.Exec(`UPDATE planning_interactions SET status='answered' WHERE task_id=? AND status='queued'`, out.TaskID); err != nil {
		return err
	}
	w.observeReport = fakeReportObserver
	ack, err := w.acceptReport(t.Context(), r)
	if err != nil {
		return err
	}
	if !ack.Accepted {
		t.Fatal("report not accepted")
	}
	out.State = runtime.RunWaiting
	out.Suspend = &runtime.SuspendRequest{TurnID: r.TurnID, ToolCallID: r.ToolCallID, Kind: "approval", Prompt: p.Spec}
	if kind == reportprotocol.Clarify {
		out.Suspend.Kind = "question"
	}
	out.AnswerSubject = "bonnie.answers." + out.AgentID
	return w.result(t.Context(), out)
}

func submitBuildReport(t *testing.T, w *Workflow, target client.Target, number int) reportprotocol.Request {
	t.Helper()
	r, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: target.TaskID, AgentID: target.AgentID, RunID: target.RunID, AttemptID: target.AttemptID, TurnID: "turn", ToolCallID: "call"}, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: number})
	if err != nil {
		t.Fatal(err)
	}
	w.observeReport = fakeReportObserver
	ack, err := w.acceptReport(t.Context(), r)
	if err != nil || !ack.Accepted {
		t.Fatalf("typed report: %+v %v", ack, err)
	}
	return r
}

func storedPlanningWaiting(t *testing.T, w *Workflow, task string) client.Outcome {
	t.Helper()
	var raw string
	if err := w.s.db.QueryRow(`SELECT waiting_json FROM planning_interactions WHERE task_id=? AND status IN ('pending','queued')`, task).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var out client.Outcome
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
