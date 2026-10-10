package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// reportPR accepts a positive PR number in a JSON object, optionally wrapped in
// one Markdown JSON fence. Provider evidence, not this report, grants readiness.
func reportPR(response string) int {
	text := strings.TrimSpace(response)
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		if len(lines) < 3 || (strings.TrimSpace(lines[0]) != "```json" && strings.TrimSpace(lines[0]) != "```") || strings.TrimSpace(lines[len(lines)-1]) != "```" {
			return 0
		}
		text = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	}
	var report struct {
		PRNumber int `json:"pr_number"`
	}
	if json.Unmarshal([]byte(text), &report) != nil || report.PRNumber < 1 {
		return 0
	}
	return report.PRNumber
}

// projectPRURL derives the link from project configuration, never worker input.
func projectPRURL(provider, repo string, number int) (string, error) {
	host, owner, name, err := providerRepository(repo)
	if err != nil {
		return "", err
	}
	kind := "pull"
	switch provider {
	case "github":
		if host != "github.com" {
			return "", fmt.Errorf("provider: only github.com is supported")
		}
	case "forgejo":
		kind = "pulls"
	default:
		return "", fmt.Errorf("provider: unsupported provider")
	}
	if number < 1 {
		return "", fmt.Errorf("provider: invalid PR number")
	}
	return fmt.Sprintf("https://%s/%s/%s/%s/%d", host, owner, name, kind, number), nil
}
