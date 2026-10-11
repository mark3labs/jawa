package reporttools

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"jawa/internal/reportprotocol"
)

func TestModelCannotOverrideTrustedIdentity(t *testing.T) {
	trusted := reportprotocol.Identity{TaskID: "trusted-task", AgentID: "trusted-agent", RunID: "trusted-run", AttemptID: "trusted-attempt", TurnID: "trusted-turn", ToolCallID: "model-call"}
	var submitted reportprotocol.Request
	tool := CompleteTask(Binding{Identity: func(context.Context) (reportprotocol.Identity, error) { return trusted, nil }, Submit: func(_ context.Context, r reportprotocol.Request) (reportprotocol.Ack, error) {
		submitted = r
		return reportprotocol.Ack{Version: 1, ReportID: r.ReportID, Accepted: true}, nil
	}})
	_, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "model-call", Name: "completeTask", Input: `{"pr_number":7,"task_id":"spoof","agent_id":"spoof","run_id":"spoof","attempt_id":"spoof","turn_id":"spoof","tool_call_id":"spoof"}`})
	if err != nil {
		t.Fatal(err)
	}
	if submitted.Identity != trusted {
		t.Fatalf("model changed trusted identity: %+v", submitted.Identity)
	}
	for _, tool := range Tools(Binding{}) {
		if strings.Contains(tool.Info().Description, "task_id") {
			t.Fatal("tool advertises identity params")
		}
	}
}
func TestUnboundDiscoveryRegistrationFailsClosed(t *testing.T) {
	response, err := CompleteTask(Binding{}).Run(context.Background(), fantasy.ToolCall{ID: "model", Name: "completeTask", Input: `{"pr_number":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if !response.IsError {
		t.Fatal("unbound tool accepted report")
	}
}
