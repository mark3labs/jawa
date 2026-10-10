package orchestrator

import (
	"encoding/json"
	"strings"
)

// reportPR accepts a JSON object, optionally wrapped in one Markdown JSON fence.
// Prose, multiple blocks, and trailing content remain invalid. Provider evidence,
// not this report, is what grants readiness.
func reportPR(response string) string {
	text := strings.TrimSpace(response)
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		if len(lines) < 3 || (strings.TrimSpace(lines[0]) != "```json" && strings.TrimSpace(lines[0]) != "```") || strings.TrimSpace(lines[len(lines)-1]) != "```" {
			return ""
		}
		text = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	}
	var report struct {
		PRURL string `json:"pr_url"`
	}
	if json.Unmarshal([]byte(text), &report) != nil || !validPR(report.PRURL) {
		return ""
	}
	return report.PRURL
}
