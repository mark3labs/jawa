package main

import (
	"encoding/json"
	"io"

	"github.com/charmbracelet/log"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// redactedActivity logs run lifecycle with IDs and states only; model text,
// prompts and tool payloads never reach operational logs. The underlying logger
// is safe for concurrent use and must not be reconfigured while logging.
type redactedActivity struct{ logger *log.Logger }

var _ runtime.ActivityLogger = redactedActivity{}

func newRedactedActivity(w io.Writer) redactedActivity {
	return redactedActivity{logger: log.NewWithOptions(w, log.Options{ReportTimestamp: true})}
}

func (l redactedActivity) LogActivity(ev runtime.Event) {
	fields := []any{"run_id", ev.RunID, "turn_id", ev.TurnID, "seq", ev.Seq}
	level := log.DebugLevel
	switch ev.Type {
	case runtime.EventState:
		level = log.InfoLevel
		fields = append(fields, "state", ev.State)
		if ev.State == runtime.RunFailed {
			level = log.ErrorLevel
		}
	case runtime.EventSuspend, runtime.EventResume, runtime.EventResponse:
		level = log.InfoLevel
	case string(kit.EventToolCall), string(kit.EventToolResult):
		level = log.InfoLevel
		var tool struct {
			ToolCallID string
			ToolName   string
			IsError    bool
		}
		if json.Unmarshal(ev.Data, &tool) == nil {
			fields = append(fields, "tool", tool.ToolName, "tool_call_id", tool.ToolCallID)
			if tool.IsError {
				level = log.ErrorLevel
			}
		}
	case string(kit.EventError):
		level = log.ErrorLevel
	case string(kit.EventWarnings), string(kit.EventRetry):
		level = log.WarnLevel
	}
	l.logger.Log(level, ev.Type, fields...)
}
