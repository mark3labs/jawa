package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	bonnieclient "github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func TestRunSectionsAndSummaries(t *testing.T) {
	for _, c := range [][3]string{
		{"custom", `{"action":"inspect"}`, "inspect"},
		{"custom", `{"other":"huge"}`, "other: huge"},
		{"custom", `{}`, ""},
		{"custom", "invalid", ""},
		{"shell_wait", `{"job_id":"ed68178e24ea9d5a"}`, "ed68178e"},
		{"read_file", `{"path":"go.mod"}`, "go.mod"},
		{"list_files", `{}`, "."},
		{"shell", `{"command":"go test ./..."}`, "go test ./..."},
		{"read", `{"path":"a.go","offset":10,"limit":5}`, "a.go · lines 10–14"},
		{"write", `{"path":"b.go","content":"x\ny"}`, "b.go · 2 lines"},
		{"edit", `{"path":"c.go","edits":[{},{}]}`, "c.go · 2 edits"},
		{"edit", `{"path":"c.go","edits":[{}]}`, "c.go"},
		{"grep", `{"pattern":"TODO","path":"internal"}`, "TODO in internal"},
		{"ls", `{}`, "."},
	} {
		if got := toolSummary(c[0], c[1]); got != c[2] {
			t.Errorf("toolSummary(%s, %s) = %q, want %q", c[0], c[1], got, c[2])
		}
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
	if messages[1].Parts[0].Tool.Name != "Tool result" || messages[1].Parts[0].Paired || !messages[1].chatVisible() || !strings.Contains(messages[1].Raw, "literal output") {
		t.Fatal("unmatched result lost")
	}
}

func TestRunToolResultsPairWithCalls(t *testing.T) {
	messages := projectRunMessages([]kit.LLMMessage{
		{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMToolCallPart{ToolCallID: "a", ToolName: "shell", Input: `{"command":"go test"}`}, kit.LLMToolCallPart{ToolCallID: "b", ToolName: "edit", Input: `{"path":"x.go"}`}, kit.LLMToolCallPart{ToolCallID: "c", ToolName: "read", Input: `{"path":"y.go"}`}}},
		{Role: "tool", Content: []kit.LLMMessagePart{
			kit.LLMToolResultPart{ToolCallID: "a", Output: kit.LLMToolResultOutputContentError{Error: errors.New("FAIL x\nExit code: 2")}},
			kit.LLMToolResultPart{ToolCallID: "b", Output: kit.LLMToolResultOutputContentText{Text: "Applied edit to x.go\n--- x.go\n+++ x.go\n@@ -1 +1 @@\n-<old>\n+new"}},
		}},
	})
	shell, edit, read := messages[0].Parts[0].Tool, messages[0].Parts[1].Tool, messages[0].Parts[2].Tool
	if shell.Result == nil || !shell.Result.Error || shell.Result.Status != "exit 2" || shell.Result.Summary != "FAIL x" {
		t.Fatalf("shell result not paired or labelled: %+v", shell.Result)
	}
	if edit.Result == nil || edit.Result.Error || edit.Result.Summary != "Applied edit to x.go" || len(edit.Result.Sections) != 2 || !edit.Result.Sections[1].Diff {
		t.Fatalf("edit result: %+v", edit.Result)
	}
	diff := edit.Result.Sections[1].HTML
	if !strings.Contains(diff, `<span class="diff-del">-&lt;old&gt;</span>`) || !strings.Contains(diff, `<span class="diff-add">+new</span>`) || !strings.Contains(diff, `class="diff-hunk"`) {
		t.Fatal("diff not escaped and classified", diff)
	}
	if read.Result != nil {
		t.Fatal("result invented for pending call")
	}
	if messages[1].chatVisible() || !messages[1].Parts[0].Paired {
		t.Fatal("paired results still render separately in Chat")
	}
	if !strings.Contains(messages[1].preview(), "shell · result") {
		t.Fatal("Trace lost paired result", messages[1].preview())
	}
	var out bytes.Buffer
	if err := runChatEntry(runDetail{}, runChatEntryView{Message: messages[0], Role: "assistant", ShowRole: true}).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"run-tool-error", `data-status="error"`, "Failed to run", "exit 2", "Edited", "+1 −1", "Applied edit to x.go", `data-status="pending"`, "Reading", "no result", "run-diff", messages[0].Parts[0].Key + "-result-0-code", `data-slot="trace"`, `data-slot="scroll-area"`} {
		if !strings.Contains(html, want) {
			t.Errorf("Chat entry missing %q", want)
		}
	}
	if strings.Contains(html, "Raw payload") || strings.Contains(html, "Message content") {
		t.Fatal("raw payload disclosures belong in Trace")
	}
	if statuses := outputSections(`{"stdout":"ok","stderr":"warn"}`); len(statuses) != 2 {
		t.Fatal("stdout/stderr sections")
	}
	stderr := &runTool{Sections: outputSections(`{"stdout":"ok","stderr":"warn"}`)}
	resultInfo("custom", stderr)
	if stderr.Error || !stderr.Stderr || stderr.Status != "stderr" {
		t.Fatalf("stderr is a warning, not a failure: %+v", stderr)
	}
	file := &runTool{Sections: outputSections("line\nExit code: 3")}
	resultInfo("read", file)
	if file.Error || file.Summary != "2 lines" {
		t.Fatalf("file content parsed as an exit code: %+v", file)
	}
}

func TestRunReasoningAndAttachments(t *testing.T) {
	messages := projectRunMessages([]kit.LLMMessage{{Role: "assistant", Content: []kit.LLMMessagePart{
		kit.LLMReasoningPart{Text: "Weigh **both** options"},
		kit.LLMReasoningPart{Text: "  "},
		kit.LLMToolCallPart{ToolCallID: "a", ToolName: "completeTask", Input: `{"pr_number":239}`},
		kit.LLMFilePart{Filename: "shot.png", MediaType: "image/png", Data: []byte("x")},
		kit.LLMTextPart{Text: "Done."},
	}}})
	parts := messages[0].Parts
	if len(parts) != 4 || !parts[0].Reasoning || parts[2].Attachment != "shot.png" || parts[3].Text != "Done." {
		t.Fatalf("parts: %+v", parts)
	}
	for _, p := range parts {
		if p.Unsupported {
			t.Fatal("known part rendered as unsupported")
		}
	}
	if blocks := messages[0].blocks(); len(blocks) != 2 || len(blocks[0]) != 3 {
		t.Fatalf("steps not grouped into one Trace: %d blocks", len(blocks))
	}
	if s := toolStep(parts[1].Tool, false); s.Verb != "Reporting completion" || s.Description != "no result" {
		t.Fatalf("report step: %+v", s)
	}
	var out bytes.Buffer
	if err := runParts(messages[0], false).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"Thought", "<strong>both</strong>", "Attached", "shot.png", "Done."} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, "Unsupported content") || strings.Count(html, `data-slot="trace"`) != 1 {
		t.Fatal("Trace grouping", html)
	}
}

func TestRunToolStepTitles(t *testing.T) {
	for _, c := range []struct {
		name, input, status, want string
	}{
		{"shell", `{"command":"make"}`, "", "Ran make"},
		{"shell", `{"command":"make"}`, "running", "Running make"},
		{"shell", `{"command":"make"}`, "error", "Failed to run make"},
		{"shell_wait", `{"job_id":"ed68178e24ea"}`, "", "Waited for job ed68178e"},
		{"edit", `{"path":"x.go"}`, "", "Edited x.go"},
		{"grep", `{"pattern":"TODO"}`, "", "Searched for TODO"},
		{"completeTask", `{"pr_number":3}`, "", "Reported completion"},
		{"custom_tool", `{"query":"q"}`, "", "Called custom_tool q"},
	} {
		s := newStep(c.name, c.input, c.status)
		if got := strings.TrimSpace(s.Verb + " " + s.Object); got != c.want || s.Icon == nil {
			t.Errorf("%s/%s = %q, want %q", c.name, c.status, got, c.want)
		}
	}
	if s := newStep("clarify", `{"questions":["a","b"]}`, ""); s.Description != "2 questions" {
		t.Fatal(s)
	}
	if s := newStep("completeTask", `{"pr_number":3}`, ""); s.Description != "PR #3" {
		t.Fatal(s)
	}
	job := &runTool{Sections: outputSections(`{"job_id":"j","running":true,"completed":false,"stdout":"building"}`)}
	resultInfo("shell", job)
	if job.Error || job.Status != "still running" {
		t.Fatalf("background job: %+v", job)
	}
	compact := &runTool{Sections: []runSection{newRunSection("Output", "{\n  \"a\": 1\n}")}}
	resultInfo("custom", compact)
	if compact.Summary != `{"a":1}` {
		t.Fatalf("JSON summarized by its first line: %q", compact.Summary)
	}
}

// ScrollArea renders a random root ID; the wrapper makes it stable without
// touching child content.
func TestRunScrollAreaIsDeterministic(t *testing.T) {
	render := func() string {
		var out bytes.Buffer
		children := templ.Raw("<pre>base-ui-id-ABCDEFGHIJKLMNOPQRSTUVWXYZ</pre>")
		if err := runScrollArea("k", "run-scroll").Render(templ.WithChildren(t.Context(), children), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	first := render()
	if first != render() || !strings.Contains(first, `data-id="base-ui-k-viewport"`) || !strings.Contains(first, `data-id="base-ui-k-scrollbar"`) {
		t.Fatal("nondeterministic scroll area", first)
	}
	if !strings.Contains(first, "base-ui-id-ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Fatal("child content rewritten")
	}
	if !strings.Contains(first, `data-slot="scroll-area-scrollbar" data-preserve-attr="hidden style"`) || !strings.Contains(first, `data-slot="scroll-area-thumb" data-preserve-attr="style"`) {
		t.Fatal("runtime scrollbar state not preserved across morphs")
	}
}

func TestRunMarkdownCodeCopyAndCache(t *testing.T) {
	messages := projectRunMessages([]kit.LLMMessage{{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "```\n<b>one</b>\n```\n\ntext\n\n    two"}}}})
	html := messages[0].Parts[0].HTML
	key := messages[0].Parts[0].Key
	for _, want := range []string{`<pre id="` + key + `-code-0">`, `<pre id="` + key + `-code-1">`, `data-templ-target-id="` + key + `-code-0"`, "&lt;b&gt;one"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in %s", want, html)
		}
	}
	if strings.Count(html, `<div class="run-code">`) != 2 || strings.Count(html, "</pre></div>") != 2 {
		t.Fatal("unbalanced code wrappers", html)
	}
	if again := projectRunMessages([]kit.LLMMessage{{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "```\n<b>one</b>\n```\n\ntext\n\n    two"}}}}); again[0].Parts[0].HTML != html {
		t.Fatal("nondeterministic code copy markup")
	}
	if withCodeCopy("<p>plain</p>", "k") != "<p>plain</p>" {
		t.Fatal("prose changed")
	}
}

func TestRunLiveActivity(t *testing.T) {
	data := func(v any) []byte { b, _ := json.Marshal(v); return b }
	running := &bonnieclient.Snapshot{State: runtime.RunRunning}
	live := []runtime.Event{
		{Type: string(kit.EventMessageUpdate), Data: data(kit.MessageUpdateEvent{Chunk: "Hello <b>"})},
		{Type: string(kit.EventMessageUpdate), Data: data(kit.MessageUpdateEvent{Chunk: "world"})},
		{Type: string(kit.EventToolExecutionStart), Data: data(kit.ToolExecutionStartEvent{ToolCallID: "a", ToolName: "shell", ToolArgs: `{"command":"make"}`})},
		{Type: string(kit.EventToolExecutionStart), Data: data(kit.ToolExecutionStartEvent{ToolCallID: "b", ToolName: "read", ToolArgs: `{"path":"x"}`})},
		{Type: string(kit.EventToolResult), Data: data(kit.ToolResultEvent{ToolCallID: "a", ToolName: "shell", IsError: true})},
	}
	v := liveActivity(runObservation{Snapshot: running, Live: live})
	if !v.Working || v.Truncated || len(v.Items) != 3 {
		t.Fatalf("%+v", v)
	}
	if v.Items[0].Kind != "text" || v.Items[0].Text != "Hello <b>world" {
		t.Fatalf("streamed text: %+v", v.Items[0])
	}
	if shell := v.Items[1]; shell.Kind != "tool" || !shell.Done || !shell.Error || liveStep(shell).Verb != "Failed to run" || liveStep(shell).Object != "make" {
		t.Fatalf("tool lifecycle: %+v", shell)
	}
	if read := liveStep(v.Items[2]); read.Status != "running" || read.Verb != "Reading" {
		t.Fatalf("running tool: %+v", read)
	}
	if got := len(v.blocks()); got != 2 {
		t.Fatalf("text and steps not grouped: %d blocks", got)
	}
	thinking := liveActivity(runObservation{Snapshot: running, Live: []runtime.Event{
		{Type: string(kit.EventReasoningDelta), Data: data(kit.ReasoningDeltaEvent{Delta: "consider "})},
		{Type: string(kit.EventReasoningDelta), Data: data(kit.ReasoningDeltaEvent{Delta: "options"})},
		{Type: string(kit.EventMessageUpdate), Data: data(kit.MessageUpdateEvent{Chunk: "\n"})},
	}})
	if len(thinking.Items) != 1 || thinking.Items[0].Kind != "reasoning" || thinking.Items[0].Text != "consider options" {
		t.Fatalf("live thinking: %+v", thinking.Items)
	}
	if liveActivity(runObservation{Snapshot: running, Unreachable: true}).Working {
		t.Fatal("unreachable run shown as working")
	}
	if !liveActivity(runObservation{}).empty() {
		t.Fatal("idle run has live activity")
	}
	many := make([]runtime.Event, maxRunEvents)
	if !liveActivity(runObservation{Live: many}).Truncated {
		t.Fatal("bounded live text not marked truncated")
	}
	var out bytes.Buffer
	if err := runLive(runDetail{Observation: runObservation{Snapshot: running, Live: live}}).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if !strings.Contains(html, "Hello &lt;b&gt;world") || !strings.Contains(html, "Working…") || strings.Contains(html, "<b>") || !strings.Contains(html, "shimmer") {
		t.Fatal("live bubble", html)
	}
}

func TestRunTraceSourceProjection(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.FixedZone("offset", 3600))
	events := []runtime.Event{{TurnID: "real-turn", Seq: 5, Type: "tool_delta", Text: "first", Time: now}, {Seq: 5, Type: "tool_delta", Text: "second"}, {TurnID: "real-turn", Seq: 5, Type: "state", State: runtime.RunRunning, Data: []byte(`{"collapsed":"needle"}`)}}
	table := eventTimeline(nil, events)
	if strings.Join(table.Columns, ",") != "Source,Sequence,Time (UTC),Kind,State,Summary" {
		t.Fatal("inferred or missing column", table.Columns)
	}
	if table.Rows[0].Cells[0] != "Live" || table.Rows[0].Cells[2] != "2026-10-14 11:00:00 UTC" || table.Rows[1].Cells[2] != "" || table.Rows[1].Turn != "" {
		t.Fatal("fabricated timestamp/turn")
	}
	if table.Rows[0].Summary != "first" || table.Rows[1].Summary != "second" || !strings.Contains(table.Rows[2].Raw, "needle") {
		t.Fatal("source order or full payload lost")
	}
	same := eventTimeline(nil, append(events, events[0]))
	if same.Rows[0].Key == same.Rows[3].Key || same.Rows[0].Key != table.Rows[0].Key {
		t.Fatal("unstable occurrence identity")
	}
	empty := eventTimeline(nil, []runtime.Event{{}})
	if strings.Join(empty.Columns, ",") != "Source,Summary" {
		t.Fatal("empty metadata scaffold", empty.Columns)
	}
	merged := eventTimeline([]runtime.Event{{Seq: 1, Text: "d1"}, {Seq: 5, Text: "d5"}, {Seq: 7, Text: "d7"}}, []runtime.Event{{Seq: 5, Text: "l5a"}, {Seq: 5, Text: "l5b"}, {Seq: 6, Text: "l6"}})
	var order []string
	for _, row := range merged.Rows {
		order = append(order, row.Summary)
	}
	if strings.Join(order, ",") != "d1,d5,l5a,l5b,l6,d7" {
		t.Fatal("sources not merged by sequence", order)
	}
}

// Filtering reads entry text, so each payload is rendered once.
func TestRunTraceDoesNotDuplicatePayloads(t *testing.T) {
	d := runDetail{Tab: "trace", Observation: runObservation{Durable: []runtime.Event{{TurnID: "t", Seq: 1, Type: "run_state", Text: "unique-needle-payload"}}}}
	var out bytes.Buffer
	if err := runDetailContent(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if n := strings.Count(html, "unique-needle-payload"); n != 2 {
		t.Fatalf("payload rendered %d times, want summary and raw only", n)
	}
	for _, want := range []string{`data-trace-source="durable"`, `data-trace-turn="` + traceTurnKey("t") + `"`, "data-trace-turn-header", "$traceDurable"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s", want)
		}
	}
}
