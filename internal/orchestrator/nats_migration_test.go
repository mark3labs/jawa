package orchestrator

import (
	"slices"
	"testing"

	protocol "github.com/mark3labs/bonnie/channel/nats"
	"github.com/nats-io/nats.go"
)

func TestAgentStreamMigrationPreservesMessages(t *testing.T) {
	s, nc := workflowFixture(t)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	name := protocol.DefaultInputStreamName("bonnie.tasks")
	_, err = js.AddStream(&nats.StreamConfig{Name: name, Subjects: []string{"bonnie.tasks", "bonnie.answers.*", "bonnie.tasks.worker.*"}, MaxMsgs: 1234})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish("bonnie.tasks.worker.old", []byte("retained fixture")); err != nil {
		t.Fatal(err)
	}
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := migrateAgentStream(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	info, err := js.StreamInfo(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 || info.Config.MaxMsgs != 1234 || !slices.Contains(info.Config.Subjects, "bonnie.tasks.agent.*") {
		t.Fatalf("migration lost stream data/config: %+v", info)
	}
	if _, err := js.Publish("bonnie.tasks.agent.new", []byte("new fixture")); err != nil {
		t.Fatal(err)
	}
}
