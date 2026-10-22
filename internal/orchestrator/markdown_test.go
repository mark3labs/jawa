package orchestrator

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestPlanningMarkdownFormatting(t *testing.T) {
	t.Parallel()
	source := "# Plan\n\n## Details\n\n**bold** and *emphasis*, ~~removed~~ and `inline`\n\n- first\n- second\n\n1. ordered\n2. next\n\n> quoted\n\n```go\nif a < b { print(\"<script>\") }\n```\n\n| Name | Value |\n| --- | --- |\n| foo | bar |\n\n[docs](https://example.com/docs)"
	got := renderPlanningMarkdown(source)
	for _, want := range []string{"<h1>Plan</h1>", "<h2>Details</h2>", "<strong>bold</strong>", "<em>emphasis</em>", "<del>removed</del>", "<code>inline</code>", "<ul>", "<ol>", "<blockquote>", "<pre><code>", "a &lt; b", "&lt;script&gt;", "<table>", "<th>Name</th>", "<td>bar</td>", `href="https://example.com/docs"`, "nofollow", "noreferrer"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestPlanningMarkdownUntrustedContent(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		`<script>alert(1)</script><img src=x onerror=alert(1)>`,
		`<a href="javascript:alert(1)" onclick="alert(1)">click</a>`,
		`<svg onload="alert(1)"><foreignObject>bad</foreignObject></svg>`,
		`<iframe srcdoc="<script>alert(1)</script>"></iframe>`,
		`[bad](javascript:alert%281%29) [data](data:text/html;base64,PHNjcmlwdD4=) [vb](vbscript:alert%281%29)`,
		`[encoded](jav&#x61;script:alert%281%29)`,
		`![tracking](https://example.com/pixel.png)`,
		"```html\n<script>alert(1)</script>\n<img onerror=alert(1)>\n```",
		"`<script>alert(1)</script>`",
	} {
		t.Run(source, func(t *testing.T) {
			got := renderPlanningMarkdown(source)
			doc, err := html.Parse(strings.NewReader(got))
			if err != nil {
				t.Fatal(err)
			}
			var check func(*html.Node)
			check = func(n *html.Node) {
				if n.Type == html.ElementNode {
					switch n.Data {
					case "script", "img", "svg", "iframe", "style", "input":
						t.Errorf("unsafe element %s in %s", n.Data, got)
					}
					for _, a := range n.Attr {
						if strings.HasPrefix(a.Key, "on") || a.Key == "style" || a.Key == "srcdoc" {
							t.Errorf("unsafe attribute: %+v", a)
						}
						if a.Key == "href" && (strings.Contains(strings.ToLower(a.Val), "javascript:") || strings.Contains(a.Val, "data:") || strings.Contains(a.Val, "vbscript:")) {
							t.Errorf("unsafe href: %s", a.Val)
						}
					}
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					check(c)
				}
			}
			check(doc)
		})
	}
}

func TestPlanningMarkdownSafeLinks(t *testing.T) {
	t.Parallel()
	for _, dest := range []string{"https://example.com", "http://example.com", "mailto:hello@example.com", "/planning?card=abc", "#details"} {
		if got := renderPlanningMarkdown("[link](" + dest + ")"); !strings.Contains(got, `href="`+dest+`"`) {
			t.Errorf("lost safe link %q: %s", dest, got)
		}
	}
}

func TestPlanningMarkdownMermaidMarkers(t *testing.T) {
	t.Parallel()
	for _, fence := range []string{"```", "~~~"} {
		source := fence + "mermaid\nflowchart LR\nA[\"<script>\"] --> B\n" + fence
		got := renderPlanningMarkdown(source + "\n\n" + source)
		if strings.Count(got, `<pre><code class="language-mermaid">`) != 2 || !strings.Contains(got, "&lt;script&gt;") {
			t.Fatal(got)
		}
	}
	for _, language := range []string{"", "go", "Mermaid", "mermaid-extra", "mermaids"} {
		got := renderPlanningMarkdown("```" + language + "\nsource\n```")
		if strings.Contains(got, "class=") || !strings.Contains(got, "<pre><code>source") {
			t.Fatal(got)
		}
	}
	for _, source := range []string{`<pre><code class="language-mermaid">flowchart LR</code></pre>`, `<code class="language-mermaid" style="color:red" data-test="x">unsafe</code>`} {
		if got := renderPlanningMarkdown(source); strings.Contains(got, "language-mermaid") {
			t.Fatal(got)
		}
	}
}
