package orchestrator

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

const mermaidFence = "```mermaid\nflowchart LR\nA[Start] --> B[Done]\n```"
const mermaidSequence = "```mermaid\nsequenceDiagram\nAlice->>Bob: Hello\n```"

func TestMermaidBrowserFixture(t *testing.T) {
	dir := os.Getenv("JAWA_MERMAID_FIXTURE")
	if dir == "" {
		t.Skip("fixture export not requested")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	d := guidanceFixtureData("Building")
	d.cards[0].Description = "# Mermaid fixture\n\n**Bold** and *emphasis*. [Safe link](/planning)\n\n- List item\n\n| Name | Value |\n| --- | --- |\n| A | B |\n\n```go\nfmt.Println(1)\n```\n\n" + mermaidFence + "\n\n" + mermaidFence
	d.planning = map[string]PlanningDetail{"card": {
		Session:    PlanningSession{State: "ready", Revision: 2, Spec: mermaidSequence},
		Revisions:  []PlanningRevision{{Number: 1, Spec: mermaidFence}},
		Questions:  []string{mermaidFence},
		Discussion: []PlanningMessage{{Role: "assistant", Content: "## Assistant\n\n" + mermaidFence}, {Role: "user", Content: "**Operator**\n\n" + mermaidSequence}},
	}}
	d.workflowDiscussion[0].Reason = "**Build blocker**\n\n" + mermaidFence
	d.workflowDiscussion[0].Reply = "**Build reply**\n\n" + mermaidSequence
	write := func(name string, component templ.Component) {
		t.Helper()
		var buf bytes.Buffer
		if err := component.Render(t.Context(), &buf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("page.html", appPage(d))
	write("snapshot.html", planningContent(d))
	dialogData := d
	dialogData.cards = append([]Card(nil), d.cards...)
	dialogData.cards[0].Status = "Planning"
	detail := dialogData.planning["card"]
	detail.CanApprove = true
	dialogData.planning["card"] = detail
	write("dialog.html", assignDialog(dialogData, strings.Repeat("a", 32), dialogData.cards[0], "build", nil, nil))
	d.cards[0].Description = "# Changed\n\n" + mermaidSequence
	write("changed.html", planningContent(d))
	d.cards[0].Description = "# Failure cases\n\n```mermaid\nnot a diagram\n```\n\n```mermaid\n" + strings.Repeat("x", 50001) + "\n```"
	for _, hostile := range []string{
		`%%{init: {"securityLevel":"loose","maxEdges":999999}}%%` + "\nflowchart LR\nA-->B",
		"---\nconfig:\n  securityLevel: loose\n---\nflowchart LR\nA-->B",
		`flowchart LR` + "\n" + `A["<img src='https://evil.invalid/pixel' onerror='window.pwned=1'>"]`,
		"flowchart LR\nA-->B\nclick A \"javascript:window.pwned=1\"",
		"flowchart LR\nA-->B\nstyle A fill:url(https://evil.invalid/pixel)",
		"flowchart LR\nA@{ img: \"https://evil.invalid/pixel\" }",
		"flowchart LR\nA[\"`![tracking](/pixel)`\"]",
	} {
		d.cards[0].Description += "\n\n```mermaid\n" + hostile + "\n```"
	}
	d.cards[0].Description += "\n\n```mermaid\nflowchart LR\n" + strings.Repeat("A-->B\n", 501) + "```\n\n" + mermaidFence
	write("failures.html", planningContent(d))
	d.cards[0].Status = "Fix"
	write("fix.html", planningContent(d))
}
