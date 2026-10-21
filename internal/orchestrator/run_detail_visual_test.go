package orchestrator

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	bonnieclient "github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func runDetailFixture() runDetail {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	messages := []kit.LLMMessage{
		{Role: "user", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "Implement a readable conversation and a separate diagnostic inspector.\n\n" + strings.Repeat("Keep long prompts readable without a monospaced wall. ", 20) + "\n" + strings.Repeat("long-identifier-", 40)}}},
		{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMReasoningPart{Text: "The user wants **two** views; check the transcript first."}, kit.LLMTextPart{Text: "## Investigation\n\nI’ll inspect the implementation, preserve **drafts**, and test both views.\n\n| View | Purpose |\n| --- | --- |\n| Chat | Conversation |\n| Trace | Diagnostics |\n\n```go\n" + strings.Repeat("long_code_", 35) + "\n```"}, kit.LLMToolCallPart{ToolCallID: "call-1", ToolName: "shell", Input: `{"command":"go test ./...", "secret-fixture-search":"collapsed-needle"}`}}},
		{Role: "tool", Content: []kit.LLMMessagePart{kit.LLMToolResultPart{ToolCallID: "call-1", Output: kit.LLMToolResultOutputContentText{Text: `{"stdout":"ok jawa/internal/orchestrator", "stderr":"fixture warning", "exit_code":1}`}}}},
		{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMToolCallPart{ToolCallID: "call-2", ToolName: "read_file", Input: `{"path":"run_detail.templ"}`}}},
		{Role: "tool", Content: []kit.LLMMessagePart{kit.LLMToolResultPart{ToolCallID: "call-2", Output: kit.LLMToolResultOutputContentError{Error: errors.New("fixture error: file unavailable")}}}},
		{Role: "system", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "This is a deterministic visual fixture, not a live run."}}},
	}
	projected := projectRunMessages(messages)
	projected = append(projected, runMessage{Role: "unknown", Key: "unsupported-fixture", Raw: `{"unsupported":"preserved diagnostic payload"}`, Parts: []runPart{{Key: "unsupported-part", Unsupported: true}}})
	live := []runtime.Event{{TurnID: "turn-2", Seq: 5, Type: "tool_delta", Text: "First equal-sequence observation"}, {TurnID: "turn-2", Seq: 5, Type: "tool_delta", Text: "Second equal-sequence observation"}}
	return runDetail{MessageID: "22222222-2222-4222-8222-222222222222", Chat: []RunChatMessage{{ID: "pending-message", Text: "Queued offline message", Status: "queued", Classification: "followup"}, {ID: "failed-message", Text: "Unavailable original conversation", Status: "failed", Failure: "retired", Classification: "followup"}}, Record: buildRun(Attempt{ID: "11111111-1111-4111-8111-111111111111", Number: 1, AgentID: "fixture-agent", RunID: strings.Repeat("run-", 20), RunState: "running", Result: `{"workflow":"stored outcome, separate from conversation"}`}), Messages: projected, Policy: runPolicy{Send: true, Stop: true, Turn: "turn-2", Mode: "steer", Reason: "Message the active turn."}, Observation: runObservation{Durable: []runtime.Event{{TurnID: "turn-1", Seq: 1, Type: "run_state", Time: now, Text: "Durable fixture event"}}, Live: live, Truncated: true}}
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
			d.Policy = runPolicy{Send: true, Mode: "queue", Reason: "Messages queue while offline."}
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
	// Export the same transcript through the planning-only observation surface.
	d = runDetailFixture()
	d.Record.Mode = "Planning"
	d.Record.State = "waiting"
	d.Record.InteractionStatus = "pending"
	d.Record.InteractionKind = "propose_spec"
	d.Policy = runPolicy{Send: true, Mode: "steer", Reason: "Reply without approving the specification."}
	for _, tab := range []string{"chat", "trace"} {
		d.Tab = tab
		p := pageData{v: viewParams{View: viewRuns}, detail: &d}
		var out bytes.Buffer
		if err := appPage(p).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if path := os.Getenv("JAWA_RUN_DETAIL_HTML"); path != "" {
			if err := os.WriteFile(path+".planning-"+tab, out.Bytes(), 0600); err != nil {
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
			for _, absent := range []string{"Stored workflow deliveries", "First equal-sequence", "Tool activity", "Message content", "Raw payload"} {
				if strings.Contains(html, absent) {
					t.Errorf("Chat contains %s", absent)
				}
			}
			for _, present := range []string{"run-prose", "run-tool", "fixture error", "<strong>drafts</strong>", "run-tool-error", "exit 1", "Queued", "Failed", "run-live", "Thought", "<strong>two</strong>", "Failed to run", "go test ./..."} {
				if !strings.Contains(html, present) {
					t.Errorf("Chat missing %s", present)
				}
			}
		} else {
			for _, present := range []string{"Conversation snapshot", "Events", "Durable", "Live", "First equal-sequence", "Second equal-sequence", "collapsed-needle"} {
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

func TestRunDetailPatcherSendsOnlyChanges(t *testing.T) {
	msg := func(text string) kit.LLMMessage {
		return kit.LLMMessage{Role: "user", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: text}}}
	}
	call := kit.LLMMessage{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMToolCallPart{ToolCallID: "c", ToolName: "shell", Input: `{"command":"make"}`}}}
	idle := runObservation{Snapshot: &bonnieclient.Snapshot{State: runtime.RunCompleted}}
	d := runDetail{Tab: "chat", Messages: projectRunMessages([]kit.LLMMessage{msg("first"), call}), Observation: idle}
	var p runDetailPatcher
	frames, err := p.frames(t.Context(), d)
	if err != nil || len(frames) != 1 || !strings.Contains(frames[0], `id="run-detail-content"`) || !strings.Contains(frames[0], `id="run-detail-controls"`) {
		t.Fatal("first tick must send the full roots", frames, err)
	}
	if frames, _ = p.frames(t.Context(), d); len(frames) != 0 {
		t.Fatal("unchanged tick sent", frames)
	}
	// A result pairs into the existing call entry and a new message is appended.
	result := kit.LLMMessage{Role: "tool", Content: []kit.LLMMessagePart{kit.LLMToolResultPart{ToolCallID: "c", Output: kit.LLMToolResultOutputContentText{Text: "built"}}}}
	d.Messages = projectRunMessages([]kit.LLMMessage{msg("first"), call, result, msg("second")})
	frames, _ = p.frames(t.Context(), d)
	if len(frames) != 2 || !strings.Contains(frames[0], "data: mode append") || !strings.Contains(frames[0], "data: selector #run-messages") || !strings.Contains(frames[0], "second") || strings.Contains(frames[0], "first") {
		t.Fatal("new entry not appended alone", frames)
	}
	if !strings.Contains(frames[1], "built") || strings.Contains(frames[1], "run-detail-content") || strings.Contains(frames[1], ">first<") {
		t.Fatal("changed entry not morphed alone", frames[1])
	}
	d.Observation.Snapshot = &bonnieclient.Snapshot{State: runtime.RunRunning}
	if frames, _ = p.frames(t.Context(), d); len(frames) != 1 || !strings.Contains(frames[0], `id="run-live"`) || strings.Contains(frames[0], "run-messages") {
		t.Fatal("live change not isolated", frames)
	}
	// Compaction or any non-append change replaces the list.
	d.Messages = projectRunMessages([]kit.LLMMessage{msg("summary")})
	if frames, _ = p.frames(t.Context(), d); len(frames) != 1 || !strings.Contains(frames[0], `id="run-messages"`) || !strings.Contains(frames[0], "summary") {
		t.Fatal("rewritten transcript not replaced", frames)
	}
	d.Tab = "trace"
	var trace runDetailPatcher
	_, _ = trace.frames(t.Context(), d)
	d.Observation.Durable = []runtime.Event{{Seq: 1, Type: "run_state", Text: "trace-update"}}
	if frames, _ = trace.frames(t.Context(), d); len(frames) != 1 || !strings.Contains(frames[0], `id="run-trace"`) || !strings.Contains(frames[0], "trace-update") {
		t.Fatal("trace change not patched", frames)
	}
}

// Tabbing away closes the stream; the reconnect gets a fresh hub with no
// snapshot yet. That must not empty the viewer's transcript or Trace tables.
func TestRunDetailPatcherKeepsTranscriptWithoutSnapshot(t *testing.T) {
	msg := func(text string) kit.LLMMessage {
		return kit.LLMMessage{Role: "user", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: text}}}
	}
	observed := runObservation{Snapshot: &bonnieclient.Snapshot{State: runtime.RunCompleted}, Observed: true}
	messages := projectRunMessages([]kit.LLMMessage{msg("kept message")})
	for _, tab := range []string{"chat", "trace"} {
		var p runDetailPatcher
		loading := runDetail{Tab: tab, Observation: runObservation{Unreachable: true}}
		frames, err := p.frames(t.Context(), loading)
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range frames {
			for _, root := range []string{`id="run-detail-content"`, `id="run-messages"`, `id="run-trace"`} {
				if strings.Contains(frame, root) {
					t.Fatalf("%s: unobserved tick patched %s: %s", tab, root, frame)
				}
			}
		}
		if len(frames) != 1 || !strings.Contains(frames[0], `id="run-detail-controls"`) {
			t.Fatalf("%s: status and controls not sent: %v", tab, frames)
		}
		// The first snapshot morphs the existing list in place, not by append.
		d := runDetail{Tab: tab, Messages: messages, Observation: observed}
		frames, _ = p.frames(t.Context(), d)
		want := `id="run-messages"`
		if tab == "trace" {
			want = `id="run-trace"`
		}
		if len(frames) != 1 || strings.Contains(frames[0], "mode append") || !strings.Contains(frames[0], want) || !strings.Contains(frames[0], "kept message") {
			t.Fatalf("%s: first snapshot not morphed into place: %v", tab, frames)
		}
		// Losing the agent again leaves the transcript; regaining it with the
		// same conversation sends nothing.
		frames, _ = p.frames(t.Context(), loading)
		for _, frame := range frames {
			if strings.Contains(frame, want) {
				t.Fatalf("%s: unreachable tick replaced transcript", tab)
			}
		}
		_, _ = p.frames(t.Context(), loading)
		if frames, _ = p.frames(t.Context(), runDetail{Tab: tab, Messages: messages, Observation: observed, Record: loading.Record}); len(frames) != 1 || strings.Contains(frames[0], want) {
			t.Fatalf("%s: unchanged transcript re-sent after reconnect: %v", tab, frames)
		}
	}
}

func TestRunHubMarksFirstObservation(t *testing.T) {
	a := testApp(t)
	h, release, err := a.workflow.runAPI.observe(Attempt{AgentID: "missing-agent", RunID: "missing-run"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if h.read().Observed {
		// Possible only if the attempt already failed; still must be reachable-false.
		if !h.read().Unreachable {
			t.Fatal("observed hub without a snapshot reported reachable")
		}
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for !h.read().Observed {
		if time.Now().After(deadline) {
			t.Fatal("hub never recorded its first snapshot attempt")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if obs := h.read(); !obs.Unreachable || obs.Snapshot != nil {
		t.Fatalf("failed attempt: %+v", obs)
	}
}

func TestRunPendingDeliveryStatus(t *testing.T) {
	d := runDetail{Tab: "chat", CSRF: "csrf-fixture", Record: buildRun(Attempt{ID: "11111111-1111-4111-8111-111111111111"}), Chat: []RunChatMessage{
		{ID: "q", Text: "queued text", Status: "queued", Classification: "followup"},
		{ID: "u", Text: "uncertain text", Status: "ambiguous"},
		{ID: "d", Text: "delivered text", Status: "completed", InTranscript: true},
	}}
	var out bytes.Buffer
	if err := runPending(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"chat-delivery-q", "Queued", "Uncertain", "Reconcile uncertain message", `value="csrf-fixture"`, `name="chat_id" value="u"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(html, "delivered text") || strings.Contains(html, "followup") {
		t.Fatal("transcript duplicate or internal classification rendered")
	}
	if start, form := strings.Index(html, `id="chat-delivery-u"`), strings.Index(html, "Reconcile"); start < 0 || form < start {
		t.Fatal("reconcile control detached from its message")
	}
}

func TestRunChatExchangeMarkers(t *testing.T) {
	msg := func(role, text string) kit.LLMMessage {
		return kit.LLMMessage{Role: kit.LLMMessageRole(role), Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: text}}}
	}
	d := runDetail{Tab: "chat", Messages: projectRunMessages([]kit.LLMMessage{msg("user", "task"), msg("assistant", "ok"), msg("user", "follow")}), SentAt: map[int]time.Time{2: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}}
	var out bytes.Buffer
	if err := runMessages(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Count(html, `data-slot="marker"`) != 1 || !strings.Contains(html, "Sent 2026-10-13 12:00:00 UTC") {
		t.Fatal("exchange separators", html)
	}
}

func TestRunDetailEmptyOfflineAndErrors(t *testing.T) {
	for _, tab := range []string{"chat", "trace"} {
		d := runDetail{Tab: tab, Observation: runObservation{Unreachable: true, Truncated: true}, Record: buildRun(Attempt{Error: "<script>failure</script>"})}
		var out bytes.Buffer
		if err := runDetailContent(d).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		for _, absent := range []string{"Agent unreachable", "No conversation snapshot available", "Activity truncated", "&lt;script&gt;", "Timestamp unavailable", "run-trace-source", "Stored workflow", "not retained"} {
			if strings.Contains(html, absent) {
				t.Errorf("%s unexpectedly contains %s", tab, absent)
			}
		}
		if strings.Contains(html, "<script>") {
			t.Fatal("unescaped failure")
		}
	}
}

func TestRunDisplayUnsupportedAndFullOutput(t *testing.T) {
	messages := projectRunMessages([]kit.LLMMessage{{Role: "other", Content: []kit.LLMMessagePart{nil, kit.LLMTextPart{Text: "Following unsupported content"}}}})
	if len(messages[0].Parts) != 2 || !messages[0].Parts[0].Unsupported || messages[0].Parts[1].Text != "Following unsupported content" {
		t.Fatal("unknown part or following text lost")
	}
	sections := outputSections(`{"stdout":"ok","stderr":"warning","exit_code":1,"extra":"retained"}`)
	if len(sections) != 3 || !strings.Contains(sections[2].Text, "retained") || !strings.Contains(sections[2].Text, "exit_code") {
		t.Fatal("output metadata lost", sections)
	}
}
