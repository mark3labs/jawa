// Adapted from BONNIE web/display.go and web/tool_display.go (MIT).
// Copyright (c) 2026 Mark III Labs, LLC. See BONNIE-LICENSE.
package orchestrator

import (
	"encoding/json"
	"fmt"
	kit "github.com/mark3labs/kit/pkg/kit"
	"strings"
)

type runMessage struct {
	Role, Raw string
	Parts     []runPart
}
type runPart struct {
	Text, HTML string
	Tool       *runTool
}
type runTool struct {
	Name, Kind, Summary, Raw string
	Sections                 []runSection
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
			}
		}
		if len(sections) > 0 {
			return sections
		}
	}
	return []runSection{{"Output", text}}
}
func projectRunMessages(messages []kit.LLMMessage) []runMessage {
	names := make(map[string]string)
	out := make([]runMessage, 0, len(messages))
	for _, msg := range messages {
		m := runMessage{Role: string(msg.Role), Raw: prettyRun(msg)}
		for _, part := range msg.Content {
			switch p := part.(type) {
			case kit.LLMTextPart:
				if len(m.Parts) > 0 && m.Parts[len(m.Parts)-1].Tool == nil {
					m.Parts[len(m.Parts)-1].Text += p.Text
				} else {
					m.Parts = append(m.Parts, runPart{Text: p.Text})
				}
			case kit.LLMToolCallPart:
				names[p.ToolCallID] = p.ToolName
				m.Parts = append(m.Parts, runPart{Tool: &runTool{Name: p.ToolName, Kind: "call", Summary: toolSummary(p.Input), Raw: prettyRun(json.RawMessage(p.Input)), Sections: []runSection{{"Arguments", p.Input}}}})
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
				m.Parts = append(m.Parts, runPart{Tool: &runTool{Name: name, Kind: "result", Summary: summary, Raw: prettyRun(p), Sections: sections}})
			default:
				m.Parts = append(m.Parts, runPart{Text: fmt.Sprintf("Unsupported content (%T)", part)})
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
