// Adapted from BONNIE web/display.go and web/tool_display.go (MIT).
// Copyright (c) 2026 Mark III Labs, LLC. See BONNIE-LICENSE.
package orchestrator

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	kit "github.com/mark3labs/kit/pkg/kit"
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
type runSection struct{ Label, Text string }

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
		for _, key := range []string{"command", "path", "query", "url"} {
			var value string
			if json.Unmarshal(fields[key], &value) == nil && value != "" {
				return runPreview(value)
			}
		}
	}
	return runPreview(input)
}
func outputSections(text string) []runSection {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &fields) == nil {
		var sections []runSection
		for _, key := range []string{"stdout", "stderr"} {
			var value string
			if json.Unmarshal(fields[key], &value) == nil && value != "" {
				sections = append(sections, runSection{key, value})
				delete(fields, key)
			}
		}
		if len(sections) > 0 {
			if len(fields) > 0 {
				sections = append(sections, runSection{"Output metadata", prettyRun(fields)})
			}
			return sections
		}
	}
	return []runSection{{"Output", text}}
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
				m.Parts = append(m.Parts, runPart{Tool: &runTool{CallID: p.ToolCallID, Name: p.ToolName, Kind: "call", Summary: toolSummary(p.Input), Raw: prettyRun(json.RawMessage(p.Input)), Sections: []runSection{{"Arguments", p.Input}}}})
			case kit.LLMToolResultPart:
				name := names[p.ToolCallID]
				if name == "" {
					name = "Tool result"
				}
				sections := []runSection{{"Output", prettyRun(p)}}
				switch output := p.Output.(type) {
				case kit.LLMToolResultOutputContentText:
					sections = outputSections(output.Text)
				case kit.LLMToolResultOutputContentError:
					if output.Error != nil {
						sections = []runSection{{"Error", output.Error.Error()}}
					}
				}
				summary := ""
				if len(sections) > 0 {
					summary = runPreview(sections[0].Text)
				}
				m.Parts = append(m.Parts, runPart{Tool: &runTool{CallID: p.ToolCallID, Name: name, Kind: "result", Summary: summary, Raw: prettyRun(p), Sections: sections}})
			default:
				m.Parts = append(m.Parts, runPart{Text: "Unsupported content; inspect raw message in Trace", Unsupported: true})
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
func (m runMessage) toolOnly() bool {
	if len(m.Parts) == 0 {
		return false
	}
	for _, p := range m.Parts {
		if p.Tool == nil {
			return false
		}
	}
	return true
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
		return "Timestamp unavailable"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}
func runRole(role string) string {
	if role == "" {
		return "Unknown role"
	}
	return strings.ToUpper(string([]rune(role)[:1])) + string([]rune(role)[1:])
}
