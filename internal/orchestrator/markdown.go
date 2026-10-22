package orchestrator

import (
	"bytes"
	"html"
	"regexp"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// Raw HTML and unsafe URLs are disabled by goldmark's default renderer. A
// second, deliberately narrow allowlist protects the templ.Raw boundary and
// excludes images (including remote tracking), styles and interactive content.
// These configured instances are immutable after initialization and safe to share.
var planningMarkdown = goldmark.New(goldmark.WithExtensions(extension.Table, extension.Strikethrough))
var planningMarkdownPolicy = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "hr", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "li", "strong", "em", "del", "blockquote", "pre", "code", "table", "thead", "tbody", "tr", "th", "td")
	p.AllowAttrs("href", "title").OnElements("a")
	p.AllowAttrs("start").OnElements("ol")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^language-mermaid$`)).OnElements("code")
	p.AllowURLSchemes("http", "https", "mailto")
	p.AllowRelativeURLs(true)
	p.RequireParseableURLs(true)
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	return p
}()

// renderPlanningMarkdown is the only trusted-HTML boundary for planning text.
// Never pass untrusted source directly to templ.Raw.
func renderPlanningMarkdown(source string) string {
	var out bytes.Buffer
	if err := planningMarkdown.Convert([]byte(source), &out); err != nil {
		return "<p>" + html.EscapeString(source) + "</p>"
	}
	return planningMarkdownPolicy.Sanitize(out.String())
}
