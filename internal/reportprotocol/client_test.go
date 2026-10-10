package reportprotocol

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestClientRequiresMatchingCommitACK(t *testing.T) {
	s, err := server.NewServer(&server.Options{Port: -1})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer s.Shutdown()
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	request, err := NewRequest(Identity{"task", "agent", "run", "attempt", "turn"}, CompleteTask, Payload{PRNumber: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		ack     Ack
		success bool
	}{
		{"committed", Ack{Version: 1, ReportID: request.ReportID, Accepted: true}, true},
		{"duplicate", Ack{Version: 1, ReportID: request.ReportID, Accepted: true, Duplicate: true}, true},
		{"wrong-id", Ack{Version: 1, ReportID: "different", Accepted: true}, false},
		{"publish-not-commit", Ack{Version: 1, ReportID: request.ReportID}, false},
		{"scope-rejected", Ack{Version: 1, ReportID: request.ReportID, Error: "wrong mode"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := "test." + tc.name
			sub, err := nc.Subscribe(Subject(root), func(m *nats.Msg) { data, _ := json.Marshal(tc.ack); _ = m.Respond(data) })
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sub.Unsubscribe() }()
			_ = nc.Flush()
			_, err = (Client{Conn: nc, Root: root}).Submit(context.Background(), request)
			if (err == nil) != tc.success {
				t.Fatalf("success=%v err=%v", tc.success, err)
			}
		})
	}
}
func TestClientCancellationDoesNotImplyCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, _ := NewRequest(Identity{"task", "agent", "run", "attempt", "turn"}, CompleteTask, Payload{PRNumber: 1})
	if _, err := (Client{}).Submit(ctx, r); err == nil {
		t.Fatal("transport absence acknowledged")
	}
}
