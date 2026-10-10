package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/bonnie"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	client "github.com/mark3labs/bonnie/client/nats"
	presencenats "github.com/mark3labs/bonnie/presence/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
	server "github.com/nats-io/nats-server/v2/server"
	nats "github.com/nats-io/nats.go"
)

func TestLifecycleSmoke(t *testing.T) {
	if os.Getenv("JAWA_LIFECYCLE_SMOKE") != "1" {
		t.Skip("set JAWA_LIFECYCLE_SMOKE=1 to run lifecycle smoke test")
	}
	dir := t.TempDir()
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: filepath.Join(dir, "broker")})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("private NATS broker not ready")
	}
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "journal")
	work := filepath.Join(dir, "work")
	ctx := t.Context()
	start := func() (context.CancelFunc, <-chan error) {
		agentCtx, stop := context.WithCancel(ctx)
		registry, e := presencenats.New(agentCtx, nc, presencenats.Config{Bucket: "jawa_agents", TTL: 30 * time.Second, Create: true})
		if e != nil {
			t.Fatal(e)
		}
		a := bonnie.New(bonnie.WithName(agentID), bonnie.WithAddr("127.0.0.1:0"), bonnie.WithWebUI(false), bonnie.WithJournal(journal), bonnie.WithInstructions(""), bonnie.WithSkills(""), bonnie.WithContextFiles(""), bonnie.WithSandbox(sandbox.Local(sandbox.WithLocalRoot(work))), bonnie.WithoutHumanInput(), bonnie.WithKit(func(o *kit.Options) {
			o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.Quiet = true, true, true, true, true, true
			kit.WithProvider("smoke", func(context.Context, *kit.ProviderConfig, string) (*kit.ProviderResult, error) {
				return &kit.ProviderResult{Model: model{}}, nil
			})(o)
			kit.WithModel("smoke/final")(o)
		}), bonnie.WithChannel(func(r *runtime.Runner) (bonnie.Channel, error) {
			return natschannel.New(r, natschannel.Config{Conn: nc, RootSubject: "bonnie", AgentID: agentID, TargetedTasks: true, CreateStream: true, EventSubject: "bonnie.events"})
		}), bonnie.WithPresence(bonnie.PresenceConfig{Registry: registry, AgentID: agentID}))
		done := make(chan error, 1)
		go func() { done <- a.Run(agentCtx) }()
		return stop, done
	}
	// Capture protocol run-state events without modifying or acknowledging them.
	events := make(chan client.StatusEvent, 32)
	sub, err := nc.Subscribe("bonnie.events", func(m *nats.Msg) {
		var e client.StatusEvent
		if json.Unmarshal(m.Data, &e) == nil {
			events <- e
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Errorf("unsubscribe lifecycle events: %v", err)
		}
	}()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	firstStop, firstDone := start()
	defer firstStop()
	task := nats.Msg{Subject: "bonnie.tasks.agent." + agentID, Data: []byte(`{"version":1,"task_id":"lifecycle-task","text":"offline lifecycle fixture"}`)}
	if _, err = js.PublishMsg(&task); err != nil {
		t.Fatal(err)
	}
	// Wait for first execution, cancel as Ctrl+C would, then restart the same journal and identity.
	first := waitRunning(t, events, "lifecycle-task")
	// The fixture model holds the real Kit call open for five seconds. Give the
	// runner time to enter that call, then interrupt it before it can finish.
	time.Sleep(time.Second)
	firstStop()
	if err := <-firstDone; err != nil {
		t.Fatalf("first agent shutdown: %v", err)
	}
	secondStop, secondDone := start()
	defer secondStop()
	second := waitNewRunning(t, events, "lifecycle-task", first.EventID)
	completed := waitState(t, events, "lifecycle-task", runtime.RunCompleted)
	secondStop()
	if err := <-secondDone; err != nil {
		t.Fatalf("second agent shutdown: %v", err)
	}
	t.Logf("first run ID=%s state=%s; resumed run ID=%s state=%s; completed=%+v", first.RunID, first.State, second.RunID, second.State, completed)
	if first.RunID == "" || second.RunID != first.RunID || second.AttemptID != first.AttemptID || second.State != runtime.RunRunning || completed.State != runtime.RunCompleted {
		t.Fatalf("expected resumed same running execution and completed turn; first=%+v second=%+v completed=%+v", first, second, completed)
	}
	if hasState(events, "lifecycle-task", runtime.RunCancelled) {
		t.Fatal("graceful agent shutdown must not checkpoint the execution as cancelled")
	}
	// Reopen the on-disk journal after the host has closed it, then inspect the
	// durable conversation tree for exactly one original user prompt and no blanks.
	j, err := runtime.OpenSQLiteJournal(journal)
	if err != nil {
		t.Fatalf("open resumed journal: %v", err)
	}
	records, err := j.Replay(context.Background(), second.RunID)
	if err != nil {
		_ = j.Close()
		t.Fatalf("replay resumed journal: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close resumed journal: %v", err)
	}
	userCount := 0
	for _, record := range records {
		if record.Kind != runtime.RecordMessage || record.Role != "user" {
			continue
		}
		if len(record.Payload) == 0 {
			t.Fatalf("user message has no payload: %+v", record)
		}
		if record.Text == "" {
			t.Fatalf("empty user message in journal record: %+v", record)
		}
		if record.Text == "offline lifecycle fixture" {
			userCount++
		}
	}
	if userCount != 1 {
		t.Fatalf("original task prompt should appear exactly once; got %d", userCount)
	}
}

func TestLifecycleReconnectSmoke(t *testing.T) {
	if os.Getenv("JAWA_LIFECYCLE_RECONNECT_SMOKE") != "1" {
		t.Skip("set JAWA_LIFECYCLE_RECONNECT_SMOKE=1 to run broker reconnect smoke test")
	}
	dir := t.TempDir()
	store := filepath.Join(dir, "broker")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	startBroker := func() *server.Server {
		ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: store})
		if err != nil {
			t.Fatal(err)
		}
		go ns.Start()
		if !ns.ReadyForConnections(10 * time.Second) {
			t.Fatal("private NATS broker not ready")
		}
		return ns
	}
	ns := startBroker()
	defer func() { ns.Shutdown() }()
	url := ns.ClientURL()
	nc, err := nats.Connect(url, nats.ReconnectWait(100*time.Millisecond), nats.MaxReconnects(-1))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	journal, work := filepath.Join(dir, "journal"), filepath.Join(dir, "work")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const taskID = "lifecycle-reconnect-task"
	start := func() (context.CancelFunc, <-chan error) {
		agentCtx, stop := context.WithCancel(ctx)
		registry, e := presencenats.New(agentCtx, nc, presencenats.Config{Bucket: "jawa_agents", TTL: 30 * time.Second, Create: true})
		if e != nil {
			t.Fatal(e)
		}
		a := bonnie.New(bonnie.WithName(agentID), bonnie.WithAddr("127.0.0.1:0"), bonnie.WithWebUI(false), bonnie.WithJournal(journal), bonnie.WithInstructions(""), bonnie.WithSkills(""), bonnie.WithContextFiles(""), bonnie.WithSandbox(sandbox.Local(sandbox.WithLocalRoot(work))), bonnie.WithoutHumanInput(), bonnie.WithKit(func(o *kit.Options) {
			o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.Quiet = true, true, true, true, true, true
			kit.WithProvider("smoke", func(context.Context, *kit.ProviderConfig, string) (*kit.ProviderResult, error) {
				return &kit.ProviderResult{Model: model{}}, nil
			})(o)
			kit.WithModel("smoke/final")(o)
		}), bonnie.WithChannel(func(r *runtime.Runner) (bonnie.Channel, error) {
			return natschannel.New(r, natschannel.Config{Conn: nc, RootSubject: "bonnie", AgentID: agentID, TargetedTasks: true, CreateStream: true, EventSubject: "bonnie.events"})
		}), bonnie.WithPresence(bonnie.PresenceConfig{Registry: registry, AgentID: agentID}))
		done := make(chan error, 1)
		go func() { done <- a.Run(agentCtx) }()
		return stop, done
	}
	events := make(chan client.StatusEvent, 64)
	sub, err := nc.Subscribe("bonnie.events", func(m *nats.Msg) {
		var e client.StatusEvent
		if json.Unmarshal(m.Data, &e) == nil {
			events <- e
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Errorf("unsubscribe reconnect events: %v", err)
		}
	}()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	stop, done := start()
	defer stop()
	task := nats.Msg{Subject: "bonnie.tasks.agent." + agentID, Data: []byte(`{"version":1,"task_id":"` + taskID + `","text":"offline reconnect fixture"}`)}
	if _, err := js.PublishMsg(&task); err != nil {
		t.Fatal(err)
	}
	first := waitRunning(t, events, taskID)
	// Stop the broker while the model call remains active, then restore the same port and JetStream store.
	ns.Shutdown()
	time.Sleep(250 * time.Millisecond)
	ns = startBroker()
	recovered := waitNewRunning(t, events, taskID, first.EventID)
	cancel()
	stop()
	if err := <-done; err != nil {
		t.Fatalf("agent shutdown: %v", err)
	}
	t.Logf("pre-disconnect run ID=%s attempt=%s; recovered run ID=%s attempt=%s", first.RunID, first.AttemptID, recovered.RunID, recovered.AttemptID)
	if first.RunID == "" || recovered.RunID != first.RunID || recovered.AttemptID != first.AttemptID || recovered.State != runtime.RunRunning {
		t.Fatalf("expected same active execution after broker reconnect; first=%+v recovered=%+v", first, recovered)
	}
	if hasState(events, taskID, runtime.RunCancelled) {
		t.Fatal("broker disconnect/reconnect must not checkpoint execution as cancelled")
	}
}

func waitState(t *testing.T, events <-chan client.StatusEvent, taskID string, state runtime.RunState) client.StatusEvent {
	t.Helper()
	timer := time.NewTimer(75 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e := <-events:
			if e.TaskID == taskID && e.State == state {
				return e
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s state", state)
			return client.StatusEvent{}
		}
	}
}

func hasState(events <-chan client.StatusEvent, taskID string, state runtime.RunState) bool {
	found := false
	for {
		select {
		case e := <-events:
			if e.TaskID == taskID && e.State == state {
				found = true
			}
		default:
			return found
		}
	}
}

func waitRunning(t *testing.T, events <-chan client.StatusEvent, taskID string) client.StatusEvent {
	return waitNewRunning(t, events, taskID, "")
}

func waitNewRunning(t *testing.T, events <-chan client.StatusEvent, taskID, previousEvent string) client.StatusEvent {
	t.Helper()
	deadline := time.After(75 * time.Second)
	for {
		select {
		case e := <-events:
			t.Logf("status event run=%s attempt=%s seq=%d state=%s event=%s", e.RunID, e.AttemptID, e.Seq, e.State, e.EventID)
			if e.TaskID == taskID && e.State == runtime.RunRunning && e.EventID != previousEvent {
				return e
			}
		case <-deadline:
			t.Fatal("timed out waiting for running event")
			return client.StatusEvent{}
		}
	}
}
