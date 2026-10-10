package orchestrator

import (
	"context"
	"encoding/json"
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
	r, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: out.TaskID, AgentID: out.AgentID, RunID: out.RunID, AttemptID: out.AttemptID, TurnID: "turn"}, reportprotocol.ProposeSpec, p)
	if err != nil {
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
	return w.result(t.Context(), out)
}

func submitBuildReport(t *testing.T, w *Workflow, target client.Target, number int) reportprotocol.Request {
	t.Helper()
	r, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: target.TaskID, AgentID: target.AgentID, RunID: target.RunID, AttemptID: target.AttemptID, TurnID: "turn"}, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: number})
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
