package orchestrator

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func runDetailFixture() runDetail {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	messages := []kit.LLMMessage{
		{Role: "user", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "Implement a readable conversation and a separate diagnostic inspector.\n\n" + strings.Repeat("Keep long prompts readable without a monospaced wall. ", 20) + "\n" + strings.Repeat("long-identifier-", 40)}}},
		{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "## Investigation\n\nI’ll inspect the implementation, preserve **drafts**, and test both views.\n\n| View | Purpose |\n| --- | --- |\n| Chat | Conversation |\n| Trace | Diagnostics |\n\n```go\n" + strings.Repeat("long_code_", 35) + "\n```"}, kit.LLMToolCallPart{ToolCallID: "call-1", ToolName: "shell", Input: `{"command":"go test ./...", "secret-fixture-search":"collapsed-needle"}`}}},
		{Role: "tool", Content: []kit.LLMMessagePart{kit.LLMToolResultPart{ToolCallID: "call-1", Output: kit.LLMToolResultOutputContentText{Text: `{"stdout":"ok jawa/internal/orchestrator", "stderr":"fixture warning", "exit_code":1}`}}}},
		{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMToolCallPart{ToolCallID: "call-2", ToolName: "read_file", Input: `{"path":"run_detail.templ"}`}}},
		{Role: "tool", Content: []kit.LLMMessagePart{kit.LLMToolResultPart{ToolCallID: "call-2", Output: kit.LLMToolResultOutputContentError{Error: errors.New("fixture error: file unavailable")}}}},
		{Role: "system", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "This is a deterministic visual fixture, not a live run."}}},
	}
	projected := projectRunMessages(messages)
	projected = append(projected, runMessage{Role: "unknown", Key: "unsupported-fixture", Raw: `{"unsupported":"preserved diagnostic payload"}`, Parts: []runPart{{Key: "unsupported-part", Text: "Unsupported content; inspect raw message in Trace"}}})
	live := []runtime.Event{{TurnID: "turn-2", Seq: 5, Type: "tool_delta", Text: "First equal-sequence observation"}, {TurnID: "turn-2", Seq: 5, Type: "tool_delta", Text: "Second equal-sequence observation"}}
	return runDetail{Attempt: Attempt{ID: "11111111-1111-4111-8111-111111111111", Number: 1, AgentID: "fixture-agent", RunID: strings.Repeat("run-", 20), RunState: "running", Result: `{"workflow":"stored outcome, separate from conversation"}`}, Messages: projected, Groups: append(groupRunEvents([]runtime.Event{{TurnID: "turn-1", Seq: 1, Type: "run_state", Time: now, Text: "Durable fixture event"}}, false), groupRunEvents(live, true)...), Stored: []storedRunEvent{{Kind: "run_state", Raw: `{"delivery":"stored-needle"}`, Time: now}}, Policy: runPolicy{Send: true, Stop: true, Turn: "turn-2", Mode: "steer", Reason: "Message the active turn."}, Observation: runObservation{Live: live, Truncated: true}}
}

func TestRunDetailVisualFixture(t *testing.T) {
	d := runDetailFixture()
	for _, variant := range []string{"chat", "trace", "offline-chat", "offline-trace"} {
		d.Tab = "chat"
		if strings.HasSuffix(variant, "trace") {
			d.Tab = "trace"
		}
		d.Observation.Unreachable = strings.HasPrefix(variant, "offline")
		if d.Observation.Unreachable {
			d.Policy = runPolicy{Reason: "Read-only. Use the card's authoritative Resume/Retry or guidance action."}
		}
		p := pageData{v: viewParams{View: viewRuns}, detail: &d, now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
		var out bytes.Buffer
		if err := appPage(p).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if path := os.Getenv("JAWA_RUN_DETAIL_HTML"); path != "" {
			if err := os.WriteFile(path+"."+variant, out.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRunDetailPresentationAndIdentity(t *testing.T) {
	d := runDetailFixture()
	ids := regexp.MustCompile(` id="([^"]+)"`)
	for _, tab := range []string{"chat", "trace"} {
		d.Tab = tab
		var out bytes.Buffer
		if err := runDetailContent(d).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		seen := map[string]bool{}
		for _, m := range ids.FindAllStringSubmatch(html, -1) {
			if seen[m[1]] {
				t.Fatalf("duplicate %s ID: %s", tab, m[1])
			}
			seen[m[1]] = true
		}
		if tab == "chat" {
			for _, absent := range []string{"Raw message", "Raw payload", "Stored workflow deliveries", "First equal-sequence"} {
				if strings.Contains(html, absent) {
					t.Errorf("Chat contains %s", absent)
				}
			}
			for _, present := range []string{"run-prose", "run-chat-activity", "fixture error", "<strong>drafts</strong>", "Unsupported content"} {
				if !strings.Contains(html, present) {
					t.Errorf("Chat missing %s", present)
				}
			}
		} else {
			for _, present := range []string{"Raw message", "Raw payload", "Timestamp unavailable", "First equal-sequence", "Second equal-sequence", "Live only, not retained", "stored-needle", "collapsed-needle"} {
				if !strings.Contains(html, present) {
					t.Errorf("Trace missing %s", present)
				}
			}
			if regexp.MustCompile(`<summary[^>]*>[^<]*0001-01-01`).MatchString(html) {
				t.Fatal("year-one timestamp")
			}
		}
		out.Reset()
		_ = runDetailContent(d).Render(t.Context(), &out)
		if html != out.String() {
			t.Fatal("nondeterministic rendering")
		}
	}
	// Identical repeated messages still have distinct, deterministic fallback keys.
	msg := kit.LLMMessage{Role: "user", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "repeat"}}}
	projected := projectRunMessages([]kit.LLMMessage{msg, msg})
	if projected[0].Key == projected[1].Key {
		t.Fatal("duplicate snapshot identity")
	}
	if projectRunMessages([]kit.LLMMessage{msg, msg})[0].Key != projected[0].Key {
		t.Fatal("unstable snapshot key")
	}
}

func TestRunDetailEmptyOfflineAndErrors(t *testing.T) {
	for _, tab := range []string{"chat", "trace"} {
		d := runDetail{Tab: tab, Observation: runObservation{Unreachable: true, Truncated: true}, Attempt: Attempt{Error: "<script>failure</script>"}}
		var out bytes.Buffer
		if err := runDetailContent(d).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		for _, present := range []string{"Agent unreachable", "No conversation snapshot available", "Activity truncated", "&lt;script&gt;"} {
			if !strings.Contains(html, present) {
				t.Errorf("%s missing %s", tab, present)
			}
		}
		if strings.Contains(html, "<script>") {
			t.Fatal("unescaped failure")
		}
		if tab == "chat" && !strings.Contains(html, "Inspect stored diagnostics in Trace") {
			t.Fatal("offline diagnostic link missing")
		}
	}
}

func TestRunDisplayUnsupportedAndFullOutput(t *testing.T) {
	messages := projectRunMessages([]kit.LLMMessage{{Role: "other", Content: []kit.LLMMessagePart{nil, kit.LLMTextPart{Text: "Following unsupported content"}}}})
	if len(messages[0].Parts) != 2 || messages[0].Parts[0].Text != "Unsupported content; inspect raw message in Trace" || messages[0].Parts[1].Text != "Following unsupported content" {
		t.Fatal("unknown part or following text lost")
	}
	sections := outputSections(`{"stdout":"ok","stderr":"warning","exit_code":1,"extra":"retained"}`)
	if len(sections) != 3 || !strings.Contains(sections[2].Text, "retained") || !strings.Contains(sections[2].Text, "exit_code") {
		t.Fatal("output metadata lost", sections)
	}
}
