package orchestrator

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Cancel only after the ready log, exercising startup and graceful shutdown
// without fixed ports or polling shared output from another goroutine.
type startupLogWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (w *startupLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if strings.Contains(w.buffer.String(), "Orchestrator ready") {
		w.cancel()
	}
	return n, err
}

func (w *startupLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func TestCommandStartupLogs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	output := &startupLogWriter{cancel: cancel}
	cmd := Command()
	cmd.SetOut(output)
	cmd.SetErr(output)
	cmd.SetArgs([]string{"--data-dir", t.TempDir(), "--listen", "127.0.0.1:0", "--nats-listen", "127.0.0.1:0"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("command: %v\n%s", err, output.String())
	}
	logs := output.String()
	for _, message := range []string{"Starting orchestrator", "Opening store", "NATS server ready", "Starting workflow", "Workflow ready", "Orchestrator ready", "http_address", "Shutting down orchestrator", "HTTP server stopped"} {
		if !strings.Contains(logs, message) {
			t.Errorf("missing %q in logs:\n%s", message, logs)
		}
	}
	if strings.Contains(logs, "127.0.0.1:0") {
		t.Errorf("logs should report bound ports, not requested port zero:\n%s", logs)
	}
	for _, secret := range []string{"jawa-internal", "password", "token"} {
		if strings.Contains(logs, secret) {
			t.Errorf("unexpected credential field %q in logs", secret)
		}
	}
}
