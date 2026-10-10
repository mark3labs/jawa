package orchestrator

import "testing"

func TestReportPR(t *testing.T) {
	raw := `{"pr_url":"https://github.com/example/repo/pull/1"}`
	for _, text := range []string{raw, "```json\n" + raw + "\n```", "```\n" + raw + "\n```", " \n```json\r\n" + raw + "\r\n```\n "} {
		if reportPR(text) != "https://github.com/example/repo/pull/1" {
			t.Fatalf("valid report rejected: %q", text)
		}
	}
	for _, text := range []string{"prefix\n" + raw, "```json\n" + raw + "\n```\nextra", "```js\n" + raw + "\n```", raw + raw, "```json\n" + raw + "\n```\n```json\n" + raw + "\n```", `{"pr_url":"javascript:alert(1)"}`} {
		if reportPR(text) != "" {
			t.Fatalf("invalid report accepted: %q", text)
		}
	}
}
