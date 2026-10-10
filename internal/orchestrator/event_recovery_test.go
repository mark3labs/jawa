package orchestrator

import (
	"encoding/json"
	"testing"

	protocol "github.com/mark3labs/bonnie/channel/nats"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
)

func TestRecoverRunningEventWithoutPinnedIdentity(t *testing.T) {
	s, nc := workflowFixture(t)
	if err := migrateWorkflow(s); err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context()}
	c := workflowCard(t, s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: protocol.DefaultEventStreamName("bonnie.events"), Subjects: []string{"bonnie.events"}}); err != nil {
		t.Fatal(err)
	}
	ev := client.StatusEvent{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", Type: "run_state", EventID: "running", Seq: 4, State: runtime.RunRunning}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish("bonnie.events", data); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := w.RecoverEvents(t.Context(), nc); err != nil {
			t.Fatal(err)
		}
	}
	a, err = w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.AgentID != "agent" || a.RunID != "run" || runLabel(a) != "Running" {
		t.Fatalf("missed execution not recovered: %+v", a)
	}
	id, err := workflowConsumerID(s)
	if err != nil {
		t.Fatal(err)
	}
	again, err := workflowConsumerID(s)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := workflowFixture(t)
	different, err := workflowConsumerID(other)
	if err != nil {
		t.Fatal(err)
	}
	if id != again || id == different {
		t.Fatal("consumer identity not stable/isolated")
	}
}
