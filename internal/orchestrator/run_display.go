// Adapted from BONNIE web/display.go and web/tool_display.go (MIT).
// Copyright (c) 2026 Mark III Labs, LLC. See BONNIE-LICENSE.
package orchestrator

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type runMessage struct {
	Role, Raw, Key string
	Parts          []runPart
}
type runPart struct {
	Text, HTML, Key string
	Tool            *runTool
	Unsupported     bool
}
type runTool struct {
	Name, Kind, Summary, Raw, CallID string
	Sections                         []runSection
}
type runSection struct{ Label, Text, HTML string }

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
func toolSummary(input string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &fields) == nil {
		for _, key := range []string{"command", "path", "query", "action", "url"} {
			var value string
			if json.Unmarshal(fields[key], &value) == nil && value != "" {
				return runPreview(value)
			}
		}
	}
	return "Expand arguments"
}
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
	return []runSection{newRunSection("Output", text)}
}
func projectRunMessages(messages []kit.LLMMessage) []runMessage {
	names := make(map[string]string)
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
				names[p.ToolCallID] = p.ToolName
				m.Parts = append(m.Parts, runPart{Tool: &runTool{CallID: p.ToolCallID, Name: p.ToolName, Kind: "call", Summary: toolSummary(p.Input), Raw: newRunSection("", p.Input).Text, Sections: []runSection{newRunSection("Arguments", p.Input)}}})
			case kit.LLMToolResultPart:
				name := names[p.ToolCallID]
				if name == "" {
					name = "Tool result"
				}
				sections := []runSection{newRunSection("Output", prettyRun(p))}
				switch output := p.Output.(type) {
				case kit.LLMToolResultOutputContentText:
					sections = outputSections(output.Text)
				case kit.LLMToolResultOutputContentError:
					if output.Error != nil {
						sections = []runSection{newRunSection("Error", output.Error.Error())}
					}
				}
				summary := ""
				if len(sections) > 0 {
					summary = runPreview(sections[0].Text)
				}
				m.Parts = append(m.Parts, runPart{Tool: &runTool{CallID: p.ToolCallID, Name: name, Kind: "result", Summary: summary, Raw: prettyRun(p), Sections: sections}})
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
					m.Parts[i].HTML = renderPlanningMarkdown(m.Parts[i].Text)
				}
			}
		}
		out = append(out, m)
	}
	return out
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

// Each source has its own optional columns and content identities. No journal
// positions, roles or turns are inferred from event names or snapshot order.
type runTraceRow struct {
	Key, Turn, Summary, Raw string
	Cells                   []string
	Stripe                  bool
}
type runTraceTable struct {
	Title   string
	Columns []string
	Rows    []runTraceRow
}

func snapshotTrace(messages []runMessage) runTraceTable {
	t := runTraceTable{Title: "Snapshot messages", Columns: []string{"Role", "Summary"}}
	for i, m := range messages {
		t.Rows = append(t.Rows, runTraceRow{Key: m.Key + "-trace", Cells: []string{runRole(m.Role)}, Summary: m.preview(), Raw: m.Raw, Stripe: i%2 == 1})
	}
	return t
}
func eventTrace(title, source string, events []runtime.Event) runTraceTable {
	t := runTraceTable{Title: title}
	columns := []string{"Sequence", "Time (UTC)", "Kind", "Role", "State"}
	present := make([]bool, len(columns))
	cells := make([][]string, len(events))
	for i, ev := range events {
		seq := ""
		if ev.Seq != 0 {
			seq = strconv.Itoa(ev.Seq)
		}
		cells[i] = []string{seq, runTime(ev.Time), ev.Type, "", string(ev.State)}
		for j, cell := range cells[i] {
			present[j] = present[j] || cell != ""
		}
	}
	for j, column := range columns {
		if present[j] {
			t.Columns = append(t.Columns, column)
		}
	}
	t.Columns = append(t.Columns, "Summary")
	occurrences := make(map[string]int)
	for i, ev := range events {
		raw := prettyRun(ev)
		row := runTraceRow{Key: displayKey(source, raw, occurrences), Turn: ev.TurnID, Summary: runPreview(ev.Text), Raw: raw, Stripe: i%2 == 1}
		if row.Summary == "" {
			row.Summary = runPreview(string(ev.Data))
		}
		if source == "durable" {
			row.Stripe = ev.Seq%2 == 1
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

// Compute searchable source text on the server rather than inspecting client DOM.
func traceSourceExpr(table runTraceTable) string {
	var text strings.Builder
	for _, row := range table.Rows {
		for _, cell := range row.Cells {
			text.WriteString(cell)
			text.WriteByte(' ')
		}
		text.WriteString(row.Summary)
		text.WriteByte(' ')
		text.WriteString(row.Raw)
		text.WriteByte(' ')
	}
	return "$traceFilter === '' || " + strconv.Quote(strings.ToLower(text.String())) + ".includes($traceFilter.toLowerCase())"
}
