package reportbridge

import (
	"encoding/json"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
)

func TestUnblockAuthorizationBoundary(t *testing.T) {
	target := client.Target{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "remote"}
	blocker, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "remote", TurnID: "old", ToolCallID: "call"}, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	record := func(kind string, v any) runtime.Record {
		raw, _ := json.Marshal(v)
		return runtime.Record{ExtType: kind, Payload: raw}
	}
	records := []runtime.Record{record(intentType, blocker), record(receiptType, reportprotocol.Ack{Version: 1, ReportID: blocker.ReportID, Accepted: true})}
	req := reportprotocol.FailedResume{Version: 1, ID: "command", Target: target, Seq: 10, TurnID: "old", Mode: "unblock", Generation: 1, Prompt: "finish", BlockerReportID: blocker.ReportID}
	if err := validateUnblockReports(records, req); err != nil {
		t.Fatal(err)
	}
	if err := validateUnblockReports(records[:1], req); err == nil {
		t.Fatal("unresolved intent superseded")
	}
	wrong := req
	wrong.BlockerReportID = "other"
	if err := validateUnblockReports(records, wrong); err == nil {
		t.Fatal("wrong blocker superseded")
	}
	records = append(records, record(unblockIntentType, req), runtime.Record{Kind: runtime.RecordTurn, Seq: 12, Text: "new"})
	superseded, err := supersededBlockers(records, "new")
	if err != nil || !superseded[blocker.ReportID] {
		t.Fatal(superseded, err)
	}
	if _, err := supersededBlockers(records, "old"); err == nil {
		t.Fatal("old turn admitted")
	}
	complete, _ := reportprotocol.NewRequest(blocker.Identity, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 1})
	records = []runtime.Record{record(intentType, complete), record(receiptType, reportprotocol.Ack{Version: 1, ReportID: complete.ReportID, Accepted: true})}
	if err := validateUnblockReports(records, req); err == nil {
		t.Fatal("completion reopened")
	}
}
