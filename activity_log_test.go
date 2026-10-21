package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/log"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestRedactedActivity(t *testing.T) {
	const secret = "SECRET-sentinel"
	for _, tc := range []struct {
		name  string
		event runtime.Event
		level string
		want  []string
	}{
		{"response", runtime.Event{Type: runtime.EventResponse}, "INFO", nil},
		{"state", runtime.Event{Type: runtime.EventState, State: runtime.RunRunning}, "INFO", []string{"state=running"}},
		{"failed", runtime.Event{Type: runtime.EventState, State: runtime.RunFailed}, "ERROR", []string{"state=failed"}},
		{"suspend", runtime.Event{Type: runtime.EventSuspend}, "INFO", nil},
		{"resume", runtime.Event{Type: runtime.EventResume}, "INFO", nil},
		{"call", runtime.Event{Type: string(kit.EventToolCall), Data: []byte(`{"ToolCallID":"call-1","ToolName":"shell","ToolArgs":"SECRET-sentinel","ParsedArgs":{"command":"SECRET-sentinel"}}`)}, "INFO", []string{"tool=shell", "tool_call_id=call-1"}},
		{"result", runtime.Event{Type: string(kit.EventToolResult), Data: []byte(`{"ToolCallID":"call-1","ToolName":"shell","Result":"SECRET-sentinel","IsError":false}`)}, "INFO", []string{"tool=shell", "tool_call_id=call-1"}},
		{"tool error", runtime.Event{Type: string(kit.EventToolResult), Data: []byte(`{"ToolCallID":"call-1","ToolName":"shell","Result":"SECRET-sentinel","IsError":true}`)}, "ERROR", []string{"tool=shell"}},
		{"malformed call", runtime.Event{Type: string(kit.EventToolCall), Data: []byte(`{"Arguments":"SECRET-sentinel"`)}, "INFO", nil},
		{"error", runtime.Event{Type: string(kit.EventError)}, "ERROR", nil},
		{"warnings", runtime.Event{Type: string(kit.EventWarnings)}, "WARN", nil},
		{"retry", runtime.Event{Type: string(kit.EventRetry)}, "WARN", nil},
		{"other", runtime.Event{Type: runtime.EventTurn}, "DEBUG", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := newRedactedActivity(&output)
			logger.logger.SetLevel(log.DebugLevel)
			logger.logger.SetFormatter(log.LogfmtFormatter)
			ev := tc.event
			ev.RunID, ev.TurnID, ev.Seq, ev.Text = "run-1", "turn-1", 42, secret
			if ev.Data == nil {
				ev.Data = []byte(`{"payload":"SECRET-sentinel"}`)
			}
			original := bytes.Clone(ev.Data)
			logger.LogActivity(ev)
			if !bytes.Equal(original, ev.Data) {
				t.Fatal("logger changed shared event data")
			}
			got := output.String()
			if strings.Contains(got, secret) || strings.Contains(got, "activity_payload") {
				t.Fatal("sensitive text or raw payload logging detected")
			}
			for _, want := range append(tc.want, "run_id=run-1", "turn_id=turn-1", "seq=42", "level="+strings.ToLower(tc.level), "msg="+ev.Type) {
				if !strings.Contains(got, want) {
					t.Errorf("missing metadata %q in %q", want, got)
				}
			}
			if strings.Count(got, "\n") != 1 {
				t.Errorf("expected one log line, got %q", got)
			}
		})
	}
}

func TestRedactedActivityConcurrent(t *testing.T) {
	var output bytes.Buffer
	logger := newRedactedActivity(&output)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			logger.LogActivity(runtime.Event{RunID: "run-1", Type: runtime.EventState, State: runtime.RunRunning})
		})
	}
	wg.Wait()
	if got := strings.Count(output.String(), "\n"); got != 20 {
		t.Errorf("got %d log lines, want 20", got)
	}
}
