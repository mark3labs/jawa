package reportbridge

import (
	"context"
	"encoding/json"
	"jawa/internal/reportprotocol"
	"testing"

	channel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/runtime"
)

func TestMissingReport(t *testing.T) {
	ctx := context.Background()
	cfg := channel.Config{RootSubject: "bonnie", AgentID: "agent"}
	id := reportprotocol.Identity{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "turn", ToolCallID: "call"}
	report := func(kind reportprotocol.Kind, payload reportprotocol.Payload, ack *reportprotocol.Ack) func(*testing.T, runtime.Journal) {
		return func(t *testing.T, j runtime.Journal) {
			req, err := reportprotocol.NewRequest(id, kind, payload)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(req)
			if err = appendExt(ctx, j, "run", intentType, data); err != nil {
				t.Fatal(err)
			}
			if ack != nil {
				ack.ReportID = req.ReportID
				data, _ = json.Marshal(ack)
				if err = appendExt(ctx, j, "run", receiptType, data); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	accepted := func() *reportprotocol.Ack {
		return &reportprotocol.Ack{Version: reportprotocol.Version, Accepted: true}
	}
	for _, tc := range []struct {
		name     string
		admitted bool
		setup    func(*testing.T, runtime.Journal)
		want     bool
	}{
		{"no admission", false, nil, false},
		{"admitted without report", true, nil, true},
		{"completed", true, report(reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 7}, accepted()), false},
		{"terminal blocker", true, report(reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "token"}, accepted()), false},
		{"guided blocker", true, report(reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "token"}, &reportprotocol.Ack{Version: reportprotocol.Version, Accepted: true, AwaitGuidance: true}), true},
		{"answered clarify", true, report(reportprotocol.Clarify, reportprotocol.Payload{Questions: []string{"Region?"}}, accepted()), true},
		{"accepted proposal", true, report(reportprotocol.ProposeSpec, reportprotocol.Payload{Spec: "# Spec", RepoCommit: "0123456789abcdef0123456789abcdef01234567"}, accepted()), false},
		{"rejected completion", true, report(reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 7}, &reportprotocol.Ack{Version: reportprotocol.Version, Error: "no PR"}), true},
		{"unconfirmed report", true, report(reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 7}, nil), false},
		{"chat followup", true, func(t *testing.T, j runtime.Journal) {
			if err := appendExt(ctx, j, "run", chatIntentType, []byte(`{"followup":true}`)); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := runtime.NewMemoryJournal()
			if _, err := j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordTurn, Text: "task"}); err != nil {
				t.Fatal(err)
			}
			if tc.admitted {
				if err := appendExt(ctx, j, admissionKey(cfg), admittedType, []byte(`{"agent_id":"agent","task_id":"task","run_id":"run","attempt_id":"attempt"}`)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.setup != nil {
				tc.setup(t, j)
			}
			b := Bridge{runner: runtime.NewRunner(j, nil), cfg: cfg}
			got, err := b.MissingReport(ctx, "run")
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	var unmanaged Bridge
	if got, err := unmanaged.MissingReport(ctx, "run"); err != nil || got {
		t.Fatalf("unmanaged run: %v, %v", got, err)
	}
}
