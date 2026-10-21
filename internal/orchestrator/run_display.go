// Adapted from BONNIE web/display.go and web/tool_display.go (MIT).
// Copyright (c) 2026 Mark III Labs, LLC. See BONNIE-LICENSE.
package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/a-h/templ"
	"github.com/axadrn/shadcn-templ/v2/components/copybutton"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type runMessage struct {
	Role, Raw, Key string
	Parts          []runPart
}

// Paired result parts are rendered inside their call in Chat; Trace keeps them.
type runPart struct {
	Text, HTML, Key     string
	Tool                *runTool
	Unsupported, Paired bool
}
type runTool struct {
	Name, Kind, Summary, Raw, CallID string
	Sections                         []runSection
	// Result is the paired result of a call. Status labels a result, e.g.
	// "exit 1"; Error marks a reported failure and Stderr diagnostic output.
	Result        *runTool
	Status        string
	Error, Stderr bool
}
type runSection struct {
	Label, Text, HTML string
	Diff              bool
}

func prettyRun(v any) string {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return "Unsupported payload"
	}
	return string(b)
}
func runPreview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	return s
}

// toolSummary names what a call acts on: the command, path or pattern for the
// core tools, otherwise the first short string argument.
func toolSummary(name, input string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &fields) != nil {
		return "Arguments"
	}
	str := func(key string) string {
		var value string
		_ = json.Unmarshal(fields[key], &value)
		return value
	}
	num := func(key string) int {
		var value int
		_ = json.Unmarshal(fields[key], &value)
		return value
	}
	path := str("path")
	switch name {
	case "shell", "bash":
		if c := str("command"); c != "" {
			return runPreview(c)
		}
	case "read":
		if path != "" {
			if offset, limit := num("offset"), num("limit"); limit > 0 {
				return runPreview(fmt.Sprintf("%s · lines %d–%d", path, max(offset, 1), max(offset, 1)+limit-1))
			} else if offset > 0 {
				return runPreview(fmt.Sprintf("%s · from line %d", path, offset))
			}
			return runPreview(path)
		}
	case "write":
		if path != "" {
			return runPreview(fmt.Sprintf("%s · %d lines", path, strings.Count(str("content"), "\n")+1))
		}
	case "edit":
		var edits []json.RawMessage
		if json.Unmarshal(fields["edits"], &edits) == nil && len(edits) > 1 {
			return runPreview(fmt.Sprintf("%s · %d edits", path, len(edits)))
		}
	case "grep", "find":
		if pattern := str("pattern"); pattern != "" {
			if path != "" {
				return runPreview(pattern + " in " + path)
			}
			return runPreview(pattern)
		}
	case "ls":
		if path == "" {
			return "."
		}
	}
	for _, key := range []string{"command", "path", "query", "pattern", "action", "url"} {
		if value := str(key); value != "" {
			return runPreview(value)
		}
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if value := str(key); value != "" {
			return runPreview(key + ": " + value)
		}
	}
	if len(fields) == 0 {
		return "No arguments"
	}
	return "Arguments"
}

var exitLine = regexp.MustCompile(`(?m)^Exit code: (-?\d+)\s*$`)

func outputSections(text string) []runSection {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &fields) == nil {
		var sections []runSection
		for _, key := range []string{"stdout", "stderr"} {
			var value string
			if json.Unmarshal(fields[key], &value) == nil && value != "" {
				sections = append(sections, newRunSection(key, value))
				delete(fields, key)
			}
		}
		if len(sections) > 0 {
			if len(fields) > 0 {
				sections = append(sections, newRunSection("Metadata", prettyRun(fields)))
			}
			return sections
		}
	}
	// Kit edit output is a status line followed by a unified diff.
	if i := strings.Index(text, "\n--- "); i >= 0 && strings.Contains(text[i:], "\n+++ ") {
		return []runSection{newRunSection("Output", text[:i]), newDiffSection(text[i+1:])}
	}
	return []runSection{newRunSection("Output", text)}
}

// resultInfo labels a result from its own payload: error outputs, nonzero exit
// codes and stderr. The summary prefers the last output line of commands.
func resultInfo(name string, t *runTool) {
	var exit string
	for _, s := range t.Sections {
		switch s.Label {
		case "stderr":
			t.Stderr = true
		case "Metadata":
			var meta struct {
				ExitCode *int `json:"exit_code"`
			}
			if json.Unmarshal([]byte(s.Text), &meta) == nil && meta.ExitCode != nil && *meta.ExitCode != 0 {
				exit = strconv.Itoa(*meta.ExitCode)
			}
		}
		if m := exitLine.FindAllStringSubmatch(s.Text, -1); len(m) > 0 && exit == "" && (name == "shell" || name == "bash") {
			exit = m[len(m)-1][1]
		}
	}
	switch {
	case exit != "":
		t.Error, t.Status = true, "exit "+exit
	case t.Error:
		t.Status = "error"
	case t.Stderr:
		t.Status = "stderr"
	}
	if len(t.Sections) == 0 {
		return
	}
	text := t.Sections[0].Text
	lines := strings.Split(strings.TrimSpace(exitLine.ReplaceAllString(text, "")), "\n")
	switch {
	case name == "read" && !t.Error:
		t.Summary = fmt.Sprintf("%d lines", strings.Count(strings.TrimRight(text, "\n"), "\n")+1)
	case name == "shell" || name == "bash":
		t.Summary = runPreview(lines[len(lines)-1])
	default:
		t.Summary = runPreview(lines[0])
	}
}

// Messages are projected with the BONNIE renderer's structure: ordered text
// and tool parts. Results are paired with their call by tool call ID.
func projectRunMessages(messages []kit.LLMMessage) []runMessage {
	calls := make(map[string]*runTool)
	occurrences := make(map[string]int)
	out := make([]runMessage, 0, len(messages))
	for _, msg := range messages {
		m := runMessage{Role: string(msg.Role), Raw: prettyRun(msg)}
		m.Key = displayKey("snapshot", m.Raw, occurrences)
		for _, part := range msg.Content {
			switch p := part.(type) {
			case kit.LLMTextPart:
				if len(m.Parts) > 0 && m.Parts[len(m.Parts)-1].Tool == nil && !m.Parts[len(m.Parts)-1].Unsupported {
					m.Parts[len(m.Parts)-1].Text += p.Text
				} else {
					m.Parts = append(m.Parts, runPart{Text: p.Text})
				}
			case kit.LLMToolCallPart:
				call := &runTool{CallID: p.ToolCallID, Name: p.ToolName, Kind: "call", Summary: toolSummary(p.ToolName, p.Input), Raw: newRunSection("", p.Input).Text, Sections: []runSection{newRunSection("Arguments", p.Input)}}
				if p.ToolCallID != "" {
					calls[p.ToolCallID] = call
				}
				m.Parts = append(m.Parts, runPart{Tool: call})
			case kit.LLMToolResultPart:
				call := calls[p.ToolCallID]
				name := "Tool result"
				if call != nil {
					name = call.Name
				}
				result := &runTool{CallID: p.ToolCallID, Name: name, Kind: "result", Raw: prettyRun(p), Sections: []runSection{newRunSection("Output", prettyRun(p))}}
				switch output := p.Output.(type) {
				case kit.LLMToolResultOutputContentText:
					result.Sections = outputSections(output.Text)
				case kit.LLMToolResultOutputContentError:
					result.Error = true
					if output.Error != nil {
						result.Sections = []runSection{newRunSection("Error", output.Error.Error())}
					}
				}
				resultInfo(name, result)
				paired := call != nil && call.Result == nil
				if paired {
					call.Result = result
				}
				m.Parts = append(m.Parts, runPart{Tool: result, Paired: paired})
			default:
				m.Parts = append(m.Parts, runPart{Unsupported: true})
			}
		}
		for i := range m.Parts {
			m.Parts[i].Key = m.Key + "-part-" + fmt.Sprint(i)
			if m.Parts[i].Tool != nil && m.Parts[i].Tool.CallID != "" {
				m.Parts[i].Key += "-" + fmt.Sprintf("%x", sha256.Sum256([]byte(m.Parts[i].Tool.CallID)))[:12]
			}
		}
		if m.Role == "assistant" {
			for i := range m.Parts {
				if m.Parts[i].Tool == nil {
					m.Parts[i].HTML = withCodeCopy(runMarkdown(m.Parts[i].Text), m.Parts[i].Key)
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// chatVisible reports whether Chat has anything to show for the message once
// paired results are folded into their calls.
func (m runMessage) chatVisible() bool {
	for _, p := range m.Parts {
		if !p.Paired && (p.Tool != nil || p.Unsupported || p.Text != "") {
			return true
		}
	}
	return false
}

// Committed messages are re-projected on every stream tick, so rendered
// Markdown is cached by content. The cache is bounded and reset when full.
var runMarkdownCache = struct {
	sync.Mutex
	html map[[32]byte]string
}{html: make(map[[32]byte]string)}

func runMarkdown(text string) string {
	key := sha256.Sum256([]byte(text))
	runMarkdownCache.Lock()
	out, ok := runMarkdownCache.html[key]
	runMarkdownCache.Unlock()
	if ok {
		return out
	}
	out = renderPlanningMarkdown(text)
	runMarkdownCache.Lock()
	if len(runMarkdownCache.html) >= 512 {
		clear(runMarkdownCache.html)
	}
	runMarkdownCache.html[key] = out
	runMarkdownCache.Unlock()
	return out
}

// withCodeCopy wraps sanitized code blocks with a copy button. It runs after
// sanitizing, and the injected markup contains only fixed text and the key.
func withCodeCopy(sanitized, key string) string {
	if !strings.Contains(sanitized, "<pre>") {
		return sanitized
	}
	var b strings.Builder
	rest := sanitized
	for n := 0; ; n++ {
		i := strings.Index(rest, "<pre>")
		if i < 0 {
			break
		}
		id := key + "-code-" + strconv.Itoa(n)
		b.WriteString(rest[:i])
		b.WriteString(`<div class="run-code">`)
		_ = copybutton.CopyButton(copybutton.Props{ID: id + "-copy", TargetID: id, Class: "run-copy", Attrs: templ.Attributes{"aria-label": "Copy code"}}).Render(context.Background(), &b)
		b.WriteString(`<pre id="` + html.EscapeString(id) + `">`)
		rest = rest[i+len("<pre>"):]
		if j := strings.Index(rest, "</pre>"); j >= 0 {
			b.WriteString(rest[:j+len("</pre>")])
			b.WriteString("</div>")
			rest = rest[j+len("</pre>"):]
		}
	}
	b.WriteString(rest)
	return b.String()
}

// Content fingerprints plus occurrence counts avoid sequence-only live identity.
// Snapshot keys are display identities, not durable journal IDs.
func displayKey(source, raw string, occurrences map[string]int) string {
	key := fmt.Sprintf("%s-%x", source, sha256.Sum256([]byte(raw)))
	n := occurrences[key]
	occurrences[key]++
	return fmt.Sprintf("%s-%d", key, n)
}
func (m runMessage) preview() string {
	var texts []string
	for _, p := range m.Parts {
		if p.Tool != nil {
			texts = append(texts, p.Tool.Name+" · "+p.Tool.Kind+" · "+p.Tool.Summary)
		} else {
			texts = append(texts, p.Text)
		}
	}
	return runPreview(strings.Join(texts, " "))
}
func runTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}
func runRole(role string) string {
	if role == "" {
		return ""
	}
	return strings.ToUpper(string([]rune(role)[:1])) + string([]rune(role)[1:])
}

var jsonToken = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|\b(?:true|false|null)\b`)

// newRunSection formats valid JSON and colors its tokens. Plain output stays
// literal, not Markdown or HTML. All source text is escaped before templ.Raw.
func newRunSection(label, text string) runSection {
	s := runSection{Label: label, Text: text}
	if !json.Valid([]byte(text)) {
		s.HTML = html.EscapeString(text)
		return s
	}
	s.Text = prettyRun(json.RawMessage(text))
	var b strings.Builder
	end := 0
	for _, match := range jsonToken.FindAllStringIndex(s.Text, -1) {
		b.WriteString(html.EscapeString(s.Text[end:match[0]]))
		token := s.Text[match[0]:match[1]]
		class := "number"
		if token[0] == '"' {
			class = "string"
			if strings.HasPrefix(strings.TrimSpace(s.Text[match[1]:]), ":") {
				class = "key"
			}
		} else if token == "true" || token == "false" || token == "null" {
			class = "literal"
		}
		b.WriteString(`<span class="syntax-` + class + `">`)
		b.WriteString(html.EscapeString(token))
		b.WriteString("</span>")
		end = match[1]
	}
	b.WriteString(html.EscapeString(s.Text[end:]))
	s.HTML = b.String()
	return s
}

// newDiffSection colors unified diff lines; every line is escaped.
func newDiffSection(text string) runSection {
	s := runSection{Label: "Diff", Text: text, Diff: true}
	var b strings.Builder
	for i, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		class := ""
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			class = "diff-file"
		case strings.HasPrefix(line, "@@"):
			class = "diff-hunk"
		case strings.HasPrefix(line, "+"):
			class = "diff-add"
		case strings.HasPrefix(line, "-"):
			class = "diff-del"
		}
		if class == "" {
			b.WriteString(html.EscapeString(line))
			continue
		}
		b.WriteString(`<span class="` + class + `">` + html.EscapeString(line) + `</span>`)
	}
	s.HTML = b.String()
	return s
}

// runLiveView is the uncommitted tail of the current turn: streamed text and
// tool executions observed after the snapshot cursor. A committed snapshot
// replaces it, so it is provisional and never stored.
type runLiveView struct {
	Text               string
	Tools              []runLiveTool
	Working, Truncated bool
}
type runLiveTool struct {
	Key, Name, Summary string
	Done, Error        bool
}

func (v runLiveView) empty() bool { return v.Text == "" && len(v.Tools) == 0 && !v.Working }

func liveActivity(obs runObservation) runLiveView {
	v := runLiveView{Working: !obs.Unreachable && obs.Snapshot != nil && obs.Snapshot.State == runtime.RunRunning}
	// Live activity is bounded to the newest events, so early text may be gone.
	v.Truncated = len(obs.Live) >= maxRunEvents
	tools := make(map[string]int)
	var text strings.Builder
	for _, ev := range obs.Live {
		var data struct {
			Chunk, ToolCallID, ToolName, ToolArgs string
			IsError                               bool
		}
		_ = json.Unmarshal(ev.Data, &data)
		switch ev.Type {
		case string(kit.EventMessageUpdate):
			text.WriteString(data.Chunk)
		case string(kit.EventToolExecutionStart):
			if _, seen := tools[data.ToolCallID]; seen && data.ToolCallID != "" {
				continue
			}
			tools[data.ToolCallID] = len(v.Tools)
			v.Tools = append(v.Tools, runLiveTool{Key: "run-live-tool-" + strconv.Itoa(len(v.Tools)), Name: data.ToolName, Summary: toolSummary(data.ToolName, data.ToolArgs)})
		case string(kit.EventToolExecutionEnd), string(kit.EventToolResult):
			if i, ok := tools[data.ToolCallID]; ok {
				v.Tools[i].Done = true
				v.Tools[i].Error = v.Tools[i].Error || data.IsError
			}
		}
	}
	v.Text = strings.TrimLeft(text.String(), "\n")
	return v
}

// Trace rows keep each source's order and content identity. Durable and live
// events share one sequence space and are merged by it; no roles or turns are
// inferred from event names or snapshot order.
type runTraceRow struct {
	Key, Turn, Source, Summary, Raw string
	Cells                           []string
	Stripe                          bool
}
type runTraceTable struct {
	ID, Title string
	Columns   []string
	Rows      []runTraceRow
}

func snapshotTrace(messages []runMessage) runTraceTable {
	t := runTraceTable{ID: "run-trace-snapshot", Title: "Conversation snapshot", Columns: []string{"Role", "Summary"}}
	for i, m := range messages {
		t.Rows = append(t.Rows, runTraceRow{Key: m.Key + "-trace", Source: "snapshot", Cells: []string{runRole(m.Role)}, Summary: m.preview(), Raw: m.Raw, Stripe: i%2 == 1})
	}
	return t
}

// eventTimeline merges durable and live events by sequence. At an equal
// sequence durable events come first, and equal-sequence live events keep
// their arrival order.
func eventTimeline(durable, live []runtime.Event) runTraceTable {
	type sourced struct {
		source string
		ev     runtime.Event
	}
	events := make([]sourced, 0, len(durable)+len(live))
	for i, j := 0, 0; i < len(durable) || j < len(live); {
		if j == len(live) || (i < len(durable) && durable[i].Seq <= live[j].Seq) {
			events = append(events, sourced{"durable", durable[i]})
			i++
		} else {
			events = append(events, sourced{"live", live[j]})
			j++
		}
	}
	t := runTraceTable{ID: "run-trace-events", Title: "Events"}
	columns := []string{"Sequence", "Time (UTC)", "Kind", "State"}
	present := make([]bool, len(columns))
	cells := make([][]string, len(events))
	for i, e := range events {
		seq := ""
		if e.ev.Seq != 0 {
			seq = strconv.Itoa(e.ev.Seq)
		}
		cells[i] = []string{seq, runTime(e.ev.Time), e.ev.Type, string(e.ev.State)}
		for j, cell := range cells[i] {
			present[j] = present[j] || cell != ""
		}
	}
	t.Columns = []string{"Source"}
	for j, column := range columns {
		if present[j] {
			t.Columns = append(t.Columns, column)
		}
	}
	t.Columns = append(t.Columns, "Summary")
	occurrences := make(map[string]int)
	for i, e := range events {
		raw := prettyRun(e.ev)
		row := runTraceRow{Key: displayKey(e.source, raw, occurrences), Turn: e.ev.TurnID, Source: e.source, Summary: runPreview(e.ev.Text), Raw: raw, Stripe: i%2 == 1, Cells: []string{runRole(e.source)}}
		if row.Summary == "" {
			row.Summary = runPreview(string(e.ev.Data))
		}
		for j, cell := range cells[i] {
			if present[j] {
				row.Cells = append(row.Cells, cell)
			}
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

func traceStripe(stripe bool) string {
	if stripe {
		return "run-stripe"
	}
	return ""
}

// Filtering reads each entry's own text, including collapsed payloads, so no
// searchable copy of the payloads is rendered. Source toggles are signals
// outside the patch roots.
const traceSources = "{snapshot: $traceSnapshot, durable: $traceDurable, live: $traceLive}"

func traceRowExpr(source string) string {
	return "(" + traceSources + ")['" + source + "'] && (" + traceMatch + ")"
}

// traceGroupExpr shows a section or turn separator while any of its entries
// is shown. Signals are read outside the callback so they are always tracked.
func traceGroupExpr(scope, selector string) string {
	return "((f, s) => [...el" + scope + ".querySelectorAll('" + selector + "')].some(r => s[r.dataset.traceSource] && (f === '' || r.textContent.toLowerCase().includes(f))))($traceFilter.toLowerCase(), " + traceSources + ")"
}
func traceTurnKey(turn string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(turn)))[:12]
}
