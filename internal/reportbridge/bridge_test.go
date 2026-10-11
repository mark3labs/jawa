package reportbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
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
		{"clarify", `{"questions":["Which region?"]}`, reportprotocol.Clarify},
		{"proposeSpec", `{"spec":"Inspect and implement","repo_commit":"` + strings.Repeat("a", 40) + `"}`, reportprotocol.ProposeSpec},
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
			if out.Error != "" || out.State != expectedState(tc.kind) || out.RunID != report.RunID || out.AttemptID != report.AttemptID {
				t.Fatalf("outcome: %+v", out)
			}
			if len(model.Requests()) != 1 {
				t.Fatal("accepted report did not halt real model loop")
			}
			requests := model.Requests()
			for _, name := range []string{"clarify", "proposeSpec", "completeTask", "reportBlocker"} {
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
	original, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "original-turn", ToolCallID: "call"}, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 4})
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
	id := reportprotocol.Identity{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "turn", ToolCallID: "call"}
	first, _ := reportprotocol.NewRequest(id, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 1})
	data, _ := json.Marshal(first)
	_, err := j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordExtensionData, ExtType: intentType, Payload: data})
	if err != nil {
		t.Fatal(err)
	}
	b := Bridge{runner: runtime.NewRunner(j, nil)}
	id.ToolCallID = "other-call"
	other, _ := reportprotocol.NewRequest(id, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "changing after timeout"})
	if _, err := b.submit(ctx, other); err == nil || !strings.Contains(err.Error(), "identical payload") {
		t.Fatalf("different payload should reject before transport: %v", err)
	}
}

func expectedState(kind reportprotocol.Kind) runtime.RunState {
	if kind == reportprotocol.Clarify || kind == reportprotocol.ProposeSpec {
		return runtime.RunWaiting
	}
	return runtime.RunCompleted
}

// Actual persisted BONNIE run, real NATS Answer delivery and real KIT. Restart
// after each planning suspension, then revise and ultimately terminate.
func TestPlanningAnswerSameRunAfterRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	srv, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS not ready")
	}
	defer srv.Shutdown()
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	var reports []reportprotocol.Request
	var reportsMu sync.Mutex
	sub, err := nc.Subscribe(reportprotocol.Subject("cycle"), func(m *nats.Msg) {
		var req reportprotocol.Request
		if err := json.Unmarshal(m.Data, &req); err != nil {
			t.Error(err)
			return
		}
		if err := req.Validate(); err != nil {
			t.Error(err)
			return
		}
		reportsMu.Lock()
		reports = append(reports, req)
		reportsMu.Unlock()
		data, _ := json.Marshal(reportprotocol.Ack{Version: 1, ReportID: req.ReportID, Accepted: true})
		_ = m.Respond(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	results, err := nc.SubscribeSync("cycle.results")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = results.Unsubscribe() }()
	_ = nc.Flush()
	model := testmodel.New(testmodel.Call("clarify", `{"questions":["Which region?","Which tier?"]}`),
		testmodel.Call("proposeSpec", `{"spec":"Implement west standard","repo_commit":"`+strings.Repeat("a", 40)+`"}`),
		testmodel.Call("proposeSpec", `{"spec":"Implement west premium","repo_commit":"`+strings.Repeat("a", 40)+`"}`),
		testmodel.Call("reportBlocker", `{"reason":"Planning reviewed; build handoff required"}`))
	dir := t.TempDir()
	cfg := natschannel.Config{Conn: nc, RootSubject: "cycle", AgentID: "agent", CreateStream: true, TargetedTasks: true}
	var wrapped *reportChannel
	var runner *runtime.Runner
	var journal *runtime.SQLiteJournal
	start := func() {
		var err error
		journal, err = runtime.OpenSQLiteJournal(dir)
		if err != nil {
			t.Fatal(err)
		}
		b := &Bridge{}
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
		runner = runtime.NewRunner(journal, factory)
		ch, err := natschannel.New(runner, cfg)
		if err != nil {
			t.Fatal(err)
		}
		b.runner, b.cfg = runner, cfg
		wrapped = &reportChannel{Channel: ch, bridge: b, cfg: cfg}
		if err := wrapped.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	stop := func() {
		if err := wrapped.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
	}
	start()
	defer func() { stop() }()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(natschannel.Task{Version: 1, TaskID: "task", Text: "Inspect original repository and plan"})
	if _, err := js.Publish("cycle.tasks", data); err != nil {
		t.Fatal(err)
	}
	var first natschannel.Result
	for i := 0; i < 4; i++ {
		msg, err := results.NextMsgWithContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out natschannel.Result
		if err := json.Unmarshal(msg.Data, &out); err != nil {
			t.Fatal(err)
		}
		if out.Error != "" {
			t.Fatalf("result %d: %+v", i, out)
		}
		if i == 0 {
			first = out
		} else if out.RunID != first.RunID || out.AttemptID != first.AttemptID || out.TaskID != first.TaskID {
			t.Fatalf("answer changed admission: %+v", out)
		}
		if i == 3 {
			if out.State != runtime.RunCompleted {
				t.Fatalf("terminal: %+v", out)
			}
			break
		}
		if out.State != runtime.RunWaiting || out.Suspend == nil || out.Suspend.ToolCallID == "" {
			t.Fatalf("not suspended: %+v", out)
		}
		stop()
		start()
		snap, err := runner.Snapshot(ctx, out.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if snap.State != runtime.RunWaiting || !reflect.DeepEqual(snap.Suspend, out.Suspend) {
			t.Fatalf("lost waiting suspension: %+v", snap)
		}
		response := runtime.InputResponse{TurnID: out.Suspend.TurnID, Text: "west standard"}
		if i == 1 {
			response = runtime.Reject("Use premium instead")
			response.TurnID = out.Suspend.TurnID
		}
		if i == 2 {
			response = runtime.Approve("Reviewed")
			response.TurnID = out.Suspend.TurnID
		}
		answer := natschannel.Answer{Version: 1, MessageID: fmt.Sprintf("answer-%d", i), TaskID: out.TaskID, RunID: out.RunID, AgentID: out.AgentID, ToolCallID: out.Suspend.ToolCallID, Responses: []runtime.InputResponse{response}}
		data, _ := json.Marshal(answer)
		if _, err := js.Publish(out.AnswerSubject, data); err != nil {
			t.Fatal(err)
		}
	}
	reqs := model.Requests()
	if len(reqs) != 4 {
		t.Fatalf("model turns: %d", len(reqs))
	}
	for i, text := range []string{"west standard", "Use premium instead", "Reviewed"} {
		if !strings.Contains(reqs[i+1].Text(), text) || !strings.Contains(reqs[i+1].Text(), "Inspect original repository") {
			t.Fatalf("conversation lost at %d: %s", i, reqs[i+1].Text())
		}
	}
	reportsMu.Lock()
	defer reportsMu.Unlock()
	if len(reports) != 4 {
		t.Fatalf("reports: %+v", reports)
	}
	seen := map[string]bool{}
	for _, r := range reports {
		if seen[r.ReportID] {
			t.Fatal("distinct interactions share report ID")
		}
		seen[r.ReportID] = true
		if r.RunID != first.RunID || r.ToolCallID == "" {
			t.Fatal("untrusted identity")
		}
	}
}

func TestRepairReceiptBeforeRuntimeSuspend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j, err := runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := reportprotocol.Identity{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "turn", ToolCallID: "call"}
	req, err := reportprotocol.NewRequest(id, reportprotocol.Clarify, reportprotocol.Payload{Questions: []string{"Region?"}})
	if err != nil {
		t.Fatal(err)
	}
	appendRec := func(rec runtime.Record) {
		rec.RunID = "run"
		rec.Timestamp = time.Now().UTC()
		if _, err := j.Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	appendRec(runtime.Record{Kind: runtime.RecordTurn, Text: "turn"})
	intent, _ := json.Marshal(req)
	receipt, _ := json.Marshal(reportprotocol.Ack{Version: 1, ReportID: req.ReportID, Accepted: true})
	appendRec(runtime.Record{Kind: runtime.RecordToolIntent, Payload: []byte(`{"id":"call","name":"clarify","args":"{\"questions\":[\"Region?\"]}"}`)})
	appendRec(runtime.Record{Kind: runtime.RecordExtensionData, ExtType: intentType, Payload: intent})
	appendRec(runtime.Record{Kind: runtime.RecordExtensionData, ExtType: receiptType, Payload: receipt})
	if err := j.Checkpoint(ctx, "run", runtime.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = runtime.OpenSQLiteJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	b := Bridge{runner: runtime.NewRunner(j, nil)}
	if err := b.restorePendingSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := b.runner.Snapshot(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != runtime.RunWaiting || snap.Suspend == nil || snap.Suspend.ToolCallID != "call" || snap.Suspend.TurnID != "turn" || snap.Suspend.Prompt != "Region?" {
		t.Fatalf("receipt not repaired: %+v", snap)
	}
	recs, _ := j.Replay(ctx, "run")
	n := len(recs)
	if err := b.restorePendingSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	recs, _ = j.Replay(ctx, "run")
	if len(recs) != n {
		t.Fatal("repair was not idempotent")
	}
	s, err := runtime.Restore(ctx, "run", j)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.resolveReportTools(ctx, bonnie.RunScope{RunID: "run", Session: s}); err != nil {
		t.Fatal(err)
	}
	recs, _ = j.Replay(ctx, "run")
	found := false
	for _, rec := range recs {
		if rec.Kind == runtime.RecordToolOutcome && strings.Contains(string(rec.Payload), "Typed report committed.") {
			found = true
		}
	}
	if !found {
		t.Fatal("interrupted tool transcript lacks trusted receipt outcome")
	}
	appendRec(runtime.Record{Kind: runtime.RecordResume, Text: "west"})
	if err := j.Checkpoint(ctx, "run", runtime.RunInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := b.restorePendingSuspensions(ctx); err != nil {
		t.Fatal(err)
	}
	state, _ := j.State(ctx, "run")
	if state != runtime.RunInterrupted {
		t.Fatal("consumed interaction suspended again")
	}
}

func TestAmbiguousRetryKeepsOriginalInteraction(t *testing.T) {
	ctx := context.Background()
	j := runtime.NewMemoryJournal()
	id := reportprotocol.Identity{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "original-turn", ToolCallID: "original-call"}
	old, _ := reportprotocol.NewRequest(id, reportprotocol.Clarify, reportprotocol.Payload{Questions: []string{"Region?"}})
	data, _ := json.Marshal(old)
	_, err := j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordExtensionData, ExtType: intentType, Payload: data})
	if err != nil {
		t.Fatal(err)
	}
	b := Bridge{runner: runtime.NewRunner(j, nil)}
	id.TurnID = "recovery-turn"
	id.ToolCallID = "retry-call"
	next, _ := reportprotocol.NewRequest(id, old.Kind, old.Payload)
	prepared, err := b.prepareRetry(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Identity != old.Identity || prepared.ReportID != old.ReportID {
		t.Fatal("retry manufactured a new interaction")
	}
	receipt, _ := json.Marshal(reportprotocol.Ack{Version: 1, ReportID: old.ReportID, Accepted: true})
	_, err = j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordExtensionData, ExtType: receiptType, Payload: receipt})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = b.prepareRetry(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ReportID != next.ReportID {
		t.Fatal("resolved identical later interaction was coalesced")
	}
}
