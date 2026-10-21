package reportbridge

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	channel "github.com/mark3labs/bonnie/channel/nats"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestValidTarget(t *testing.T) {
	target := client.Target{AgentID: "agent", TaskID: "task", RunID: "run", AttemptID: "attempt"}
	for _, tc := range []struct {
		name    string
		version int
		id      string
		target  client.Target
		want    bool
	}{
		{"valid", 1, "id", target, true},
		{"max id", 1, strings.Repeat("a", 128), target, true},
		{"version", 2, "id", target, false},
		{"empty id", 1, "", target, false},
		{"long id", 1, strings.Repeat("a", 129), target, false},
		{"foreign agent", 1, "id", client.Target{AgentID: "other", TaskID: "task", RunID: "run", AttemptID: "attempt"}, false},
		{"missing task", 1, "id", client.Target{AgentID: "agent", RunID: "run", AttemptID: "attempt"}, false},
		{"missing run", 1, "id", client.Target{AgentID: "agent", TaskID: "task", AttemptID: "attempt"}, false},
		{"missing attempt", 1, "id", client.Target{AgentID: "agent", TaskID: "task", RunID: "run"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validTarget(tc.version, tc.id, tc.target, "agent"); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOwnsTarget(t *testing.T) {
	ctx := context.Background()
	cfg := channel.Config{RootSubject: "bonnie", AgentID: "agent"}
	target := client.Target{AgentID: "agent", TaskID: "task", RunID: "run", AttemptID: "attempt"}
	for _, tc := range []struct {
		name    string
		payload []byte
		extType string
		want    bool
	}{
		{"admitted", []byte(`{"agent_id":"agent","task_id":"task","run_id":"run","attempt_id":"attempt"}`), admittedType, true},
		{"foreign", []byte(`{"agent_id":"other","task_id":"task","run_id":"run","attempt_id":"attempt"}`), admittedType, false},
		{"malformed", []byte(`{`), admittedType, false},
		{"wrong type", []byte(`{"agent_id":"agent","task_id":"task","run_id":"run","attempt_id":"attempt"}`), "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := runtime.NewMemoryJournal()
			if err := appendExt(ctx, j, admissionKey(cfg), tc.extType, tc.payload); err != nil {
				t.Fatal(err)
			}
			var b Bridge
			got, err := b.ownsTarget(ctx, runtime.NewRunner(j, nil), cfg, target)
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	var b Bridge
	if _, err := b.ownsTarget(ctx, runtime.NewRunner(runtime.NewMemoryJournal(), nil), cfg, target); err == nil {
		t.Fatal("missing journal error")
	}
}

func TestSavedContext(t *testing.T) {
	records := []runtime.Record{
		{Kind: runtime.RecordContext, Payload: []byte(`{"context":["first"]}`)},
		{Kind: runtime.RecordContext, Payload: []byte(`{`)},
		{Kind: runtime.RecordMessage, Payload: []byte(`{"context":["ignored"]}`)},
	}
	lines, err := savedContext(records, false)
	if err != nil || !reflect.DeepEqual(lines, []string{"first"}) {
		t.Fatal(lines, err)
	}
	if _, err := savedContext(records, true); err == nil {
		t.Fatal("strict recovery accepted malformed context")
	}
	records = append(records, runtime.Record{Kind: runtime.RecordContext, Payload: []byte(`{"context":["last"]}`)})
	lines, err = savedContext(records, false)
	if err != nil || !reflect.DeepEqual(lines, []string{"last"}) {
		t.Fatal(lines, err)
	}
	records = append(records, runtime.Record{Kind: runtime.RecordContext, Payload: []byte(`{"context":[]}`)})
	lines, err = savedContext(records, false)
	if err != nil || len(lines) != 0 {
		t.Fatal(lines, err)
	}
}

func TestRunOutcomeAndAppendExt(t *testing.T) {
	target := client.Target{AgentID: "agent", TaskID: "task", RunID: "run", AttemptID: "attempt"}
	cfg := channel.Config{RootSubject: "bonnie", AgentID: "agent"}
	suspend := &runtime.SuspendRequest{Kind: runtime.SuspendQuestion}
	out := runOutcome(target, cfg, runtime.RunWaiting, "response", suspend)
	if out.Version != 1 || out.TaskID != target.TaskID || out.AgentID != target.AgentID || out.RunID != target.RunID || out.AttemptID != target.AttemptID || out.State != runtime.RunWaiting || out.Response != "response" || out.Suspend != suspend || out.AnswerSubject != "bonnie.answers.agent" || out.Error != "" {
		t.Fatal(out)
	}
	if out := runOutcome(target, cfg, runtime.RunCompleted, "", nil); out.AnswerSubject != "" {
		t.Fatal(out)
	}
	j := runtime.NewMemoryJournal()
	before := time.Now().UTC()
	if err := appendExt(context.Background(), j, "run", "extension", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	recs, err := j.Replay(context.Background(), "run")
	if err != nil || len(recs) != 1 {
		t.Fatal(recs, err)
	}
	rec := recs[0]
	if rec.Kind != runtime.RecordExtensionData || rec.ExtType != "extension" || string(rec.Payload) != `{}` || rec.Timestamp.Before(before) || rec.Timestamp.Location() != time.UTC {
		t.Fatal(rec)
	}
}

func TestSubscribeControl(t *testing.T) {
	s, err := server.NewServer(&server.Options{Port: -1})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS not ready")
	}
	defer s.Shutdown()
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls := make(chan string, 8)
	sub, err := subscribeControl(ctx, nc, "control", 32, func(_ context.Context, req string) any {
		calls <- req
		if req == "silent" {
			return nil
		}
		return req
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	// These are followed by a valid request on the same subscription, which
	// provides a callback-order barrier rather than relying on a sleep.
	for _, msg := range []*nats.Msg{
		{Subject: "control", Reply: "not-an-inbox", Data: []byte(`"foreign"`)},
		{Subject: "control", Reply: nats.NewInbox(), Data: []byte(`"` + strings.Repeat("x", 32) + `"`)},
		{Subject: "control", Reply: nats.NewInbox(), Data: []byte(`{`)},
	} {
		if err := nc.PublishMsg(msg); err != nil {
			t.Fatal(err)
		}
	}
	reply, err := nc.RequestWithContext(ctx, "control", []byte(`"valid"`))
	if err != nil || string(reply.Data) != `"valid"` {
		t.Fatal(reply, err)
	}
	if got := <-calls; got != "valid" {
		t.Fatalf("invalid request reached handler: %q", got)
	}
	select {
	case got := <-calls:
		t.Fatalf("unexpected handler call: %q", got)
	default:
	}
	silentCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if _, err := nc.RequestWithContext(silentCtx, "control", []byte(`"silent"`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if got := <-calls; got != "silent" {
		t.Fatal(got)
	}
}

func TestSubscribeControlFlushFailureUnsubscribes(t *testing.T) {
	s, err := server.NewServer(&server.Options{Port: -1})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS not ready")
	}
	defer s.Shutdown()
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sub, err := subscribeControl(ctx, nc, "control", 32, func(context.Context, json.RawMessage) any { return nil })
	if err == nil || sub == nil || sub.IsValid() {
		t.Fatalf("subscription not cleaned up: %v, %v", sub, err)
	}
}
