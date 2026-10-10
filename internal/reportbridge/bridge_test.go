package reportbridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"jawa/internal/reportprotocol"
	"jawa/internal/testmodel"
)

// Real NATS JetStream -> BONNIE Channel -> Runner -> real KIT scripted model ->
// bound custom tool -> request/reply. No runtime.Agent fake or identity prompts.
func TestActualNATSAgentReports(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		kind        reportprotocol.Kind
	}{
		{"proposeSpec", `{"spec":"Inspect and implement","questions":[],"repo_commit":"` + strings.Repeat("a", 40) + `"}`, reportprotocol.ProposeSpec},
		{"completeTask", `{"pr_number":23}`, reportprotocol.CompleteTask},
		{"reportBlocker", `{"reason":"missing access"}`, reportprotocol.ReportBlocker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
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
			reports := make(chan reportprotocol.Request, 4)
			sub, err := nc.Subscribe(reportprotocol.Subject("test"), func(m *nats.Msg) {
				var r reportprotocol.Request
				if err := json.Unmarshal(m.Data, &r); err != nil {
					t.Error(err)
					return
				}
				if err := r.Validate(); err != nil {
					t.Error(err)
					return
				}
				reports <- r
				data, _ := json.Marshal(reportprotocol.Ack{Version: 1, ReportID: r.ReportID, Accepted: true})
				_ = m.Respond(data)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sub.Unsubscribe() }()
			if err := nc.Flush(); err != nil {
				t.Fatal(err)
			}
			model := testmodel.New(testmodel.Call(tc.name, tc.input))
			var b Bridge
			journal := runtime.NewMemoryJournal()
			cfg := natschannel.Config{Conn: nc, RootSubject: "test", AgentID: "trusted-agent", CreateStream: true, TargetedTasks: true}
			factory := runtime.KitAgentWithSetup(false, func(ctx context.Context, k *kit.Kit, s *runtime.Session) error {
				return b.Setup(ctx, k, bonnie.RunScope{RunID: s.RunID(), Session: s})
			}, model.Option(), func(o *kit.Options) {
				o.SkipConfig = true
				o.NoContextFiles = true
				o.NoSkills = true
				o.NoExtensions = true
				o.NoAgents = true
				o.DisableCoreTools = true
				o.Quiet = true
			})
			runner := runtime.NewRunner(journal, factory)
			ch, err := natschannel.New(runner, cfg)
			if err != nil {
				t.Fatal(err)
			}
			b.runner, b.cfg = runner, cfg
			wrapped := &reportChannel{Channel: ch, bridge: &b, cfg: cfg}
			if err := wrapped.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = wrapped.Shutdown(context.Background()) }()
			results, err := nc.SubscribeSync("test.results")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = results.Unsubscribe() }()
			_ = nc.Flush()
			js, err := nc.JetStream()
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(natschannel.Task{Version: 1, TaskID: "trusted-task", Text: "Do the assigned task. Do not invent identity."})
			if _, err := js.Publish("test.tasks", data); err != nil {
				t.Fatal(err)
			}
			var report reportprotocol.Request
			select {
			case report = <-reports:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if report.TaskID != "trusted-task" || report.AgentID != "trusted-agent" || report.RunID == "" || report.AttemptID == "" || report.TurnID == "" || report.Kind != tc.kind {
				t.Fatalf("untrusted report: %+v", report)
			}
			msg, err := results.NextMsgWithContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var out natschannel.Result
			if err := json.Unmarshal(msg.Data, &out); err != nil {
				t.Fatal(err)
			}
			if out.Error != "" || out.State != runtime.RunCompleted || out.RunID != report.RunID || out.AttemptID != report.AttemptID {
				t.Fatalf("outcome: %+v", out)
			}
			if len(model.Requests()) != 1 {
				t.Fatal("accepted report did not halt real model loop")
			}
			requests := model.Requests()
			for _, name := range []string{"proposeSpec", "completeTask", "reportBlocker"} {
				if !requests[0].HasTool(name) {
					t.Fatalf("tool %s not registered", name)
				}
			}
			if strings.Contains(requests[0].Text(), report.AttemptID) {
				t.Fatal("attempt identity leaked into prompt")
			}
			snapshot, err := runner.Snapshot(ctx, report.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.TurnID != report.TurnID {
				t.Fatal("turn did not come from runtime")
			}
		})
	}
}

func TestDurableIntentRecoveryPreservesOriginalIdentity(t *testing.T) {
	ctx := context.Background()
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
	original, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "original-turn"}, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 4})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan reportprotocol.Request, 1)
	sub, err := nc.Subscribe(reportprotocol.Subject("test"), func(m *nats.Msg) {
		var request reportprotocol.Request
		_ = json.Unmarshal(m.Data, &request)
		seen <- request
		data, _ := json.Marshal(reportprotocol.Ack{Version: 1, ReportID: request.ReportID, Accepted: true, Duplicate: true})
		_ = m.Respond(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = nc.Flush()
	journal := runtime.NewMemoryJournal()
	data, _ := json.Marshal(original)
	_, err = journal.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordExtensionData, ExtType: intentType, Payload: data})
	if err != nil {
		t.Fatal(err)
	}
	b := Bridge{runner: runtime.NewRunner(journal, nil), cfg: natschannel.Config{RootSubject: "test"}, conn: nc}
	if err := b.recoverIntent(ctx, "run"); err != nil {
		t.Fatal(err)
	}
	recovered := <-seen
	if recovered.Identity != original.Identity || recovered.ReportID != original.ReportID {
		t.Fatalf("recovery changed trusted original request: %+v", recovered)
	}
	if err := b.recoverIntent(ctx, "run"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-seen:
		t.Fatal("confirmed intent redelivered")
	default:
	}
}

func TestUnknownRunFailsClosed(t *testing.T) {
	b := Bridge{runner: runtime.NewRunner(runtime.NewMemoryJournal(), nil), cfg: natschannel.Config{RootSubject: "test", AgentID: "agent"}}
	if _, err := b.identity(context.Background(), "invented-model-run"); err == nil {
		t.Fatal("unknown run accepted")
	}
}

func TestUnconfirmedIntentRejectsDifferentPayload(t *testing.T) {
	ctx := context.Background()
	j := runtime.NewMemoryJournal()
	id := reportprotocol.Identity{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "turn"}
	first, _ := reportprotocol.NewRequest(id, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 1})
	data, _ := json.Marshal(first)
	_, err := j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordExtensionData, ExtType: intentType, Payload: data})
	if err != nil {
		t.Fatal(err)
	}
	b := Bridge{runner: runtime.NewRunner(j, nil)}
	other, _ := reportprotocol.NewRequest(id, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "changing after timeout"})
	if _, err := b.submit(ctx, other); err == nil || !strings.Contains(err.Error(), "identical payload") {
		t.Fatalf("different payload should reject before transport: %v", err)
	}
}
