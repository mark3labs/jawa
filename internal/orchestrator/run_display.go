// Adapted from BONNIE web/display.go and web/tool_display.go (MIT).
// Copyright (c) 2026 Mark III Labs, LLC. See BONNIE-LICENSE.
package orchestrator

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/a-h/templ"
	"github.com/axadrn/shadcn-templ/v2/components/copybutton"
	lucide "github.com/axadrn/shadcn-templ/v2/components/icon"
	"github.com/axadrn/shadcn-templ/v2/components/scrollarea"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"html"
	"io"
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
// Reasoning parts carry model thinking text; Attachment names a file part.
type runPart struct {
	Text, HTML, Key, Attachment    string
	Tool                           *runTool
	Unsupported, Paired, Reasoning bool
}

// step reports whether the part renders as a Trace row rather than prose.
func (p runPart) step() bool { return p.Tool != nil || p.Reasoning || p.Attachment != "" }

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
		return ""
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
	case "read", "read_file":
		if path != "" {
			if offset, limit := num("offset"), num("limit"); limit > 0 {
				return runPreview(fmt.Sprintf("%s · lines %d–%d", path, max(offset, 1), max(offset, 1)+limit-1))
			} else if offset > 0 {
				return runPreview(fmt.Sprintf("%s · from line %d", path, offset))
			}
			return runPreview(path)
		}
	case "write", "write_file":
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
	case "ls", "list_files":
		if path == "" {
			return "."
		}
	case "shell_wait", "shell_status", "shell_cancel":
		if id := str("job_id"); id != "" {
			return shortID(id)
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
	return ""
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
	var running bool
	for _, s := range t.Sections {
		switch s.Label {
		case "stderr":
			t.Stderr = true
		case "Metadata":
			var meta struct {
				ExitCode *int `json:"exit_code"`
				Running  bool `json:"running"`
			}
			if json.Unmarshal([]byte(s.Text), &meta) == nil {
				if meta.ExitCode != nil && *meta.ExitCode != 0 {
					exit = strconv.Itoa(*meta.ExitCode)
				}
				running = meta.Running
			}
		}
		if m := exitLine.FindAllStringSubmatch(s.Text, -1); len(m) > 0 && exit == "" && (name == "shell" || name == "bash") {
			exit = m[len(m)-1][1]
		}
	}
	switch {
	case exit != "":
		t.Error, t.Status = true, "exit "+exit
	case running:
		t.Status = "still running"
	case t.Error:
		t.Status = "error"
	case t.Stderr:
		t.Status = "stderr"
	}
	if len(t.Sections) == 0 {
		return
	}
	text := t.Sections[0].Text
	if compact := new(bytes.Buffer); json.Compact(compact, []byte(text)) == nil {
		t.Summary = runPreview(compact.String())
		return
	}
	lines := strings.Split(strings.TrimSpace(exitLine.ReplaceAllString(text, "")), "\n")
	switch {
	case (name == "read" || name == "read_file") && !t.Error:
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
				if last := len(m.Parts) - 1; last >= 0 && !m.Parts[last].step() && !m.Parts[last].Unsupported {
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
			case kit.LLMReasoningPart:
				// Some providers send only an encrypted signature; show nothing then.
				if strings.TrimSpace(p.Text) != "" {
					m.Parts = append(m.Parts, runPart{Reasoning: true, Text: p.Text})
				}
			case kit.LLMFilePart:
				name := cmp.Or(p.Filename, p.MediaType, "file")
				m.Parts = append(m.Parts, runPart{Attachment: name})
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
				if m.Parts[i].Tool == nil && m.Parts[i].Attachment == "" {
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
		if !p.Paired && (p.step() || p.Unsupported || p.Text != "") {
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
		} else if p.Reasoning {
			texts = append(texts, "Thought · "+p.Text)
		} else if p.Attachment != "" {
			texts = append(texts, "Attachment · "+p.Attachment)
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

// runLiveView is the uncommitted tail of the current turn: streamed text,
// thinking and tool executions observed after the snapshot cursor, in arrival
// order. A committed snapshot replaces it, so it is provisional and never stored.
type runLiveView struct {
	Items              []runLiveItem
	Working, Truncated bool
}

// runLiveItem is one run of streamed text or thinking, or one tool execution.
type runLiveItem struct {
	Key, Kind, Text, Name, Input string
	Done, Error                  bool
}

func (v runLiveView) empty() bool { return len(v.Items) == 0 && !v.Working }

func liveActivity(obs runObservation) runLiveView {
	v := runLiveView{Working: obs.working()}
	// Live activity is bounded to the newest events, so early text may be gone.
	v.Truncated = len(obs.Live) >= maxRunEvents
	tools := make(map[string]int)
	stream := func(kind, chunk string) {
		if n := len(v.Items); n > 0 && v.Items[n-1].Kind == kind {
			v.Items[n-1].Text += chunk
			return
		}
		v.Items = append(v.Items, runLiveItem{Kind: kind, Text: chunk})
	}
	for _, ev := range obs.Live {
		var data struct {
			Chunk, Delta, ToolCallID, ToolName, ToolArgs string
			IsError                                      bool
		}
		_ = json.Unmarshal(ev.Data, &data)
		switch ev.Type {
		case string(kit.EventMessageUpdate):
			stream("text", data.Chunk)
		case string(kit.EventReasoningDelta):
			stream("reasoning", data.Delta)
		case string(kit.EventToolExecutionStart):
			if _, seen := tools[data.ToolCallID]; seen && data.ToolCallID != "" {
				continue
			}
			tools[data.ToolCallID] = len(v.Items)
			v.Items = append(v.Items, runLiveItem{Kind: "tool", Name: data.ToolName, Input: data.ToolArgs})
		case string(kit.EventToolExecutionEnd), string(kit.EventToolResult):
			if i, ok := tools[data.ToolCallID]; ok {
				v.Items[i].Done = true
				v.Items[i].Error = v.Items[i].Error || data.IsError
			}
		}
	}
	items := v.Items[:0]
	for _, item := range v.Items {
		if item.Kind != "tool" {
			item.Text = strings.TrimLeft(item.Text, "\n")
			if strings.TrimSpace(item.Text) == "" {
				continue
			}
		}
		item.Key = "run-live-" + strconv.Itoa(len(items))
		items = append(items, item)
	}
	v.Items = items
	return v
}

// working reports a reachable run whose observed turn is executing.
func (obs runObservation) working() bool {
	return !obs.Unreachable && obs.Snapshot != nil && obs.Snapshot.State == runtime.RunRunning
}

// runStep is one Trace line: what an agent step did, phrased for its state
// (done, running, failed or pending) as the shadcn Trace guidance describes.
type runStep struct {
	Icon                              func(...lucide.Props) templ.Component
	Verb, Object, Description, Status string
}

type stepVerbs struct {
	icon                  func(...lucide.Props) templ.Component
	done, running, failed string
}

var toolVerbs = map[string]stepVerbs{
	"shell":         {lucide.Terminal, "Ran", "Running", "Failed to run"},
	"bash":          {lucide.Terminal, "Ran", "Running", "Failed to run"},
	"shell_wait":    {lucide.Terminal, "Waited for job", "Waiting for job", "Failed waiting for job"},
	"shell_status":  {lucide.Terminal, "Checked job", "Checking job", "Failed to check job"},
	"shell_cancel":  {lucide.Terminal, "Cancelled job", "Cancelling job", "Failed to cancel job"},
	"read":          {lucide.FileText, "Read", "Reading", "Failed to read"},
	"read_file":     {lucide.FileText, "Read", "Reading", "Failed to read"},
	"write":         {lucide.FilePlus, "Wrote", "Writing", "Failed to write"},
	"write_file":    {lucide.FilePlus, "Wrote", "Writing", "Failed to write"},
	"edit":          {lucide.FilePen, "Edited", "Editing", "Failed to edit"},
	"grep":          {lucide.Search, "Searched for", "Searching for", "Failed to search for"},
	"find":          {lucide.Search, "Found files matching", "Finding files matching", "Failed to find"},
	"ls":            {lucide.FolderOpen, "Listed", "Listing", "Failed to list"},
	"list_files":    {lucide.FolderOpen, "Listed", "Listing", "Failed to list"},
	"completeTask":  {lucide.CircleCheck, "Reported completion", "Reporting completion", "Failed to report completion"},
	"reportBlocker": {lucide.TriangleAlert, "Reported a blocker", "Reporting a blocker", "Failed to report a blocker"},
	"clarify":       {lucide.ClipboardList, "Asked for clarification", "Asking for clarification", "Failed to ask for clarification"},
	"proposeSpec":   {lucide.ClipboardList, "Proposed a specification", "Proposing a specification", "Failed to propose a specification"},
}

// reportTools take their meaning from the tool, not an argument.
var reportTools = map[string]bool{"completeTask": true, "reportBlocker": true, "clarify": true, "proposeSpec": true}

// newStep phrases a tool step. status is "", "running", "error" or "pending".
func newStep(name, input, status string) runStep {
	verbs, known := toolVerbs[name]
	if !known {
		verbs = stepVerbs{lucide.Wrench, "Called", "Calling", "Failed to call"}
	}
	s := runStep{Icon: verbs.icon, Verb: verbs.done, Status: status}
	switch status {
	case "running", "pending":
		s.Verb = verbs.running
	case "error":
		s.Verb = verbs.failed
	}
	switch {
	case !known:
		s.Object = strings.TrimSpace(name + " " + toolSummary(name, input))
	case !reportTools[name]:
		s.Object = toolSummary(name, input)
	}
	var args struct {
		PRNumber  int      `json:"pr_number"`
		Questions []string `json:"questions"`
	}
	_ = json.Unmarshal([]byte(input), &args)
	switch {
	case name == "completeTask" && args.PRNumber > 0:
		s.Description = "PR #" + strconv.Itoa(args.PRNumber)
	case name == "clarify" && len(args.Questions) > 0:
		s.Description = plural(len(args.Questions), "question")
	}
	return s
}

// toolStep phrases a committed call from its paired result. A call without a
// result is running while the turn executes and pending otherwise.
func toolStep(t *runTool, working bool) runStep {
	result := t.result()
	input := ""
	if t.Kind == "call" {
		input = t.Raw
	}
	status := ""
	switch {
	case result != nil && result.Error:
		status = "error"
	case result == nil && working:
		status = "running"
	case result == nil:
		status = "pending"
	}
	name := t.Name
	if t.Kind == "result" && name == "Tool result" {
		return runStep{Icon: lucide.Wrench, Verb: "Tool result", Status: status, Description: resultDescription(name, result)}
	}
	s := newStep(name, input, status)
	if d := resultDescription(name, result); d != "" {
		s.Description = d
	} else if result == nil && !working {
		s.Description = "no result"
	}
	return s
}

func liveStep(item runLiveItem) runStep {
	status := "running"
	switch {
	case item.Error:
		status = "error"
	case item.Done:
		status = ""
	}
	return newStep(item.Name, item.Input, status)
}

// resultDescription is the secondary text of a result: an exit code, diff
// stat or line count. A failure is already said by the title.
func resultDescription(name string, r *runTool) string {
	if r == nil {
		return ""
	}
	if r.Status != "" && r.Status != "error" {
		return r.Status
	}
	for _, section := range r.Sections {
		if section.Diff {
			added, removed := 0, 0
			for _, line := range strings.Split(section.Text, "\n") {
				switch {
				case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
				case strings.HasPrefix(line, "+"):
					added++
				case strings.HasPrefix(line, "-"):
					removed++
				}
			}
			return fmt.Sprintf("+%d −%d", added, removed)
		}
	}
	if (name == "read" || name == "read_file") && !r.Error {
		return r.Summary
	}
	return ""
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

var reasoningStep = runStep{Icon: lucide.Brain, Verb: "Thought"}

func attachmentStep(name string) runStep {
	return runStep{Icon: lucide.Paperclip, Verb: "Attached", Object: name}
}

// blocks groups a message's Chat parts: consecutive steps share one Trace,
// prose parts stand alone. Paired results render inside their call.
func (m runMessage) blocks() [][]runPart {
	var out [][]runPart
	for _, p := range m.Parts {
		if p.Paired {
			continue
		}
		if n := len(out); n > 0 && p.step() && out[n-1][0].step() {
			out[n-1] = append(out[n-1], p)
			continue
		}
		out = append(out, []runPart{p})
	}
	return out
}

// liveBlocks groups consecutive live steps the same way.
func (v runLiveView) blocks() [][]runLiveItem {
	var out [][]runLiveItem
	for _, item := range v.Items {
		if n := len(out); n > 0 && item.Kind != "text" && out[n-1][0].Kind != "text" {
			out[n-1] = append(out[n-1], item)
			continue
		}
		out = append(out, []runLiveItem{item})
	}
	return out
}

// scrollAreaID matches the random root ID shadcn-templ's ScrollArea renders.
var scrollAreaID = regexp.MustCompile(`base-ui-id-[A-Z2-7]+`)

const scrollAreaChildren = "\x00run-scroll-area-children\x00"

// runScrollArea renders shadcn-templ's ScrollArea with a stable identity so
// stream ticks stay comparable: the random root ID becomes the key. Only the
// component's own markup is rewritten; children are rendered verbatim. The
// scrollbar state scrollarea.js sets survives morphs.
func runScrollArea(key, class string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var shell strings.Builder
		marker := templ.WithChildren(ctx, templ.Raw(scrollAreaChildren))
		if err := scrollarea.ScrollArea(scrollarea.Props{Class: class, Attributes: templ.Attributes{"data-preserve-attr": "style"}}).Render(marker, &shell); err != nil {
			return err
		}
		out := scrollAreaID.ReplaceAllString(shell.String(), "base-ui-"+key)
		out = strings.Replace(out, `data-slot="scroll-area-scrollbar"`, `data-slot="scroll-area-scrollbar" data-preserve-attr="hidden style"`, 1)
		out = strings.Replace(out, `data-slot="scroll-area-thumb"`, `data-slot="scroll-area-thumb" data-preserve-attr="style"`, 1)
		before, after, found := strings.Cut(out, scrollAreaChildren)
		if !found {
			return fmt.Errorf("scroll area children slot missing")
		}
		if _, err := io.WriteString(w, before); err != nil {
			return err
		}
		if children := templ.GetChildren(ctx); children != nil {
			if err := children.Render(ctx, w); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, after)
		return err
	})
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
