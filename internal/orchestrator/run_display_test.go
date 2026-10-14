package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestRunSectionsAndSummaries(t *testing.T) {
	if toolSummary(`{"action":"inspect"}`) != "inspect" || toolSummary(`{"other":"huge"}`) != "Expand arguments" || toolSummary("invalid") != "Expand arguments" {
		t.Fatal("argument summary")
	}
	for _, text := range []string{`{"html":"<img src=x onerror=alert(1)>","count":2,"ok":true}`, "<script>bad</script>", "{invalid json"} {
		s := newRunSection("Output", text)
		if strings.Contains(s.HTML, "<img") || strings.Contains(s.HTML, "<script>") {
			t.Fatal("unsafe section", s.HTML)
		}
		if !strings.Contains(s.Text, text) && !strings.Contains(s.Text, "count") {
			t.Fatal("lost literal payload")
		}
	}
	if !strings.Contains(newRunSection("", `{"count":2}`).HTML, `class="syntax-number"`) {
		t.Fatal("missing JSON highlighting")
	}
	messages := projectRunMessages([]kit.LLMMessage{{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "before"}, kit.LLMToolCallPart{ToolCallID: "a", ToolName: "shell", Input: "invalid"}, kit.LLMTextPart{Text: "after"}}}, {Role: "tool", Content: []kit.LLMMessagePart{kit.LLMToolResultPart{ToolCallID: "missing", Output: kit.LLMToolResultOutputContentText{Text: "literal output"}}}}})
	if len(messages[0].Parts) != 3 || messages[0].Parts[0].Text != "before" || messages[0].Parts[2].Text != "after" || messages[0].Parts[1].Tool.Raw != "invalid" {
		t.Fatal("ordered parts or invalid raw arguments lost")
	}
	if messages[1].Parts[0].Tool.Name != "Tool result" || !strings.Contains(messages[1].Raw, "literal output") {
		t.Fatal("unmatched result lost")
	}
}

func TestRunTraceSourceProjection(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.FixedZone("offset", 3600))
	events := []runtime.Event{{TurnID: "real-turn", Seq: 5, Type: "tool_delta", Text: "first", Time: now}, {Seq: 5, Type: "tool_delta", Text: "second"}, {TurnID: "real-turn", Seq: 5, Type: "state", State: runtime.RunRunning, Data: []byte(`{"collapsed":"needle"}`)}}
	table := eventTrace("Live events", "live", events)
	if strings.Join(table.Columns, ",") != "Sequence,Time (UTC),Kind,State,Summary" {
		t.Fatal("inferred or missing column", table.Columns)
	}
	if table.Rows[0].Cells[1] != "2026-10-14 11:00:00 UTC" || table.Rows[1].Cells[1] != "" || table.Rows[1].Turn != "" {
		t.Fatal("fabricated timestamp/turn")
	}
	if table.Rows[0].Summary != "first" || table.Rows[1].Summary != "second" || !strings.Contains(table.Rows[2].Raw, "needle") {
		t.Fatal("source order or full payload lost")
	}
	same := eventTrace("Live events", "live", append(events, events[0]))
	if same.Rows[0].Key == same.Rows[3].Key || same.Rows[0].Key != table.Rows[0].Key {
		t.Fatal("unstable occurrence identity")
	}
	empty := eventTrace("Live events", "live", []runtime.Event{{}})
	if strings.Join(empty.Columns, ",") != "Summary" {
		t.Fatal("empty metadata scaffold", empty.Columns)
	}
}
