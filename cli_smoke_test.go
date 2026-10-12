package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// TestCLISmoke exercises the actual executable, CLI parsing, authenticated NATS,
// presence and the OpenAI-compatible transport without external services.
func TestCLISmoke(t *testing.T) {
	if os.Getenv("JAWA_CLI_SMOKE") != "1" {
		t.Skip("set JAWA_CLI_SMOKE=1")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "jawa")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	broker, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: filepath.Join(dir, "broker"), Username: "smoke", Password: "smoke-password"})
	if err != nil {
		t.Fatal(err)
	}
	go broker.Start()
	if !broker.ReadyForConnections(5 * time.Second) {
		t.Fatal("broker not ready")
	}
	defer broker.Shutdown()
	nc, err := nats.Connect(broker.ClientURL(), nats.UserInfo("smoke", "smoke-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	events := make(chan client.StatusEvent, 64)
	sub, err := nc.Subscribe("cli_smoke.events", func(m *nats.Msg) {
		var event client.StatusEvent
		if json.Unmarshal(m.Data, &event) == nil {
			events <- event
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var invalid atomic.Bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "cli-smoke" || r.Header.Get("Authorization") != "Bearer smoke-api-key" {
			invalid.Store(true)
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"smoke\",\"object\":\"chat.completion.chunk\",\"model\":\"cli-smoke\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"CLI smoke task completed.\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"smoke\",\"object\":\"chat.completion.chunk\",\"model\":\"cli-smoke\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer endpoint.Close()
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("Respond briefly to the smoke task."), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "serve", "--addr=127.0.0.1:0", "--model=custom/cli-smoke", "--provider-url="+endpoint.URL+"/v1", "--provider-api-key=smoke-api-key", "--nats-url="+broker.ClientURL(), "--nats-username=smoke", "--nats-password=smoke-password", "--nats-token=", "--nats-nkey-seed=", "--nats-agent-id=cli-agent", "--nats-root-subject=cli_smoke", "--nats-consumer=cli-workers", "--nats-presence=true", "--nats-presence-bucket=cli_agents", "--sandbox-root="+filepath.Join(dir, "work"))
	cmd.Dir = dir
	// Do not inherit credentials, project config, or production connection details.
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key == "PATH" || key == "HOME" || key == "GOCACHE" || key == "GOMODCACHE" {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "JAWA_MODEL=invalid/environment-model", "NATS_URL=nats://127.0.0.1:1", "NATS_USERNAME=wrong", "NATS_PASSWORD=wrong", "NATS_TOKEN=wrong", "JAWA_NATS_PRESENCE=false")
	logFile, err := os.Create(filepath.Join(dir, "agent.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		output, _ := os.ReadFile(logFile.Name())
		t.Logf("agent log:\n%s", output)
	}()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the flag-selected presence bucket and task stream before publishing.
	for {
		_, bucketErr := js.KeyValue("cli_agents")
		_, streamErr := js.StreamNameBySubject("cli_smoke.tasks.agent.cli-agent")
		if bucketErr == nil && streamErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("startup timeout")
		case err := <-done:
			done <- err // Leave the exit result available to cleanup.
			t.Fatalf("agent exited: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if _, err := js.Publish("cli_smoke.tasks.agent.cli-agent", []byte(`{"version":1,"task_id":"cli-smoke-task","text":"Reply with a brief smoke confirmation."}`)); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-events:
			if event.TaskID == "cli-smoke-task" && event.State == runtime.RunCompleted {
				if calls.Load() == 0 || invalid.Load() {
					t.Fatal("model/key flag did not reach provider")
				}
				entries, err := os.ReadDir(filepath.Join(dir, "work"))
				if err != nil || len(entries) == 0 {
					t.Fatalf("flag-selected workspace not created: %v", err)
				}
				t.Logf("completed task=%s run=%s attempt=%s; provider requests=%d", event.TaskID, event.RunID, event.AttemptID, calls.Load())
				return
			}
			if event.State == runtime.RunFailed {
				t.Fatalf("run failed: %+v", event)
			}
		case <-ctx.Done():
			t.Fatal("task completion timeout")
		}
	}
}
