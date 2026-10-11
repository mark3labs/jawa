# Planning Markdown rendering

Planning detail renders descriptions, current/revision specs, questions, and both
assistant and user discussion text with Goldmark (CommonMark plus tables and
strikethrough). `markdown.go` is the shared trusted-HTML boundary: raw HTML remains
disabled in Goldmark and a narrow Bluemonday allowlist sanitizes generated HTML
before `templ.Raw`. Scripts, event handlers, styles, embedded HTML and images
(including remote tracking images) are not permitted. Links allow HTTP, HTTPS,
mailto and relative destinations, with nofollow/noreferrer; no new-tab target is
added. Code is escaped and displayed without executing or interpreting HTML.

`planningDiscussionMessage` renders persisted readable Markdown directly. Typed
`proposeSpec` acceptance stores a structured immutable revision (spec and commit; no questions) and constructs
readable discussion content in the same transaction. Clarify separately stores
questions as a typed interaction without creating a revision. No discussion text is decoded as JSON, repaired or interpreted as a
report. Historical arbitrary prose/JSON remains literal safe Markdown. Current
and historical revision surfaces use their persisted structured fields.

Typography is scoped to `.planning-markdown` in `frontend/src/style.css`, using
the existing night palette. Lists, headings, links, inline/fenced code,
blockquotes and tables have explicit styles after Tailwind preflight. Tables and
code blocks scroll within the content width on narrow screens.

## Regeneration and checks

From the repository root:

```sh
go run github.com/a-h/templ/cmd/templ@v0.3.1070 generate -f internal/orchestrator/planning.templ
(cd internal/orchestrator/frontend && npm run build)
go test ./...
go vet ./...
golangci-lint run ./...
```

The pinned generator matches go.mod. Only planning's generated template is
included; unrelated generator-version churn is excluded. Renderer tests cover
Markdown elements, escaped code, safe links, dangerous schemes, raw HTML and
tracking images. Template tests cover planning surfaces, structured persisted
revisions, readable discussions, literal natural/user messages and approval controls.
