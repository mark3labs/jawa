# Provider verification

The production orchestrator installs `workflow.SetVerifier(ProviderVerifier(store, nil))` at startup; nil uses the default HTTP transport. The existing workflow polls blocked completed attempts, including after restart; the adapter fetches fresh evidence on every invocation and never caches readiness. The engine does not revalidate attempts already moved to Done.

## Configuration and credentials

- GitHub: `JAWA_GITHUB_TOKEN`, falling back to `GITHUB_TOKEN` when unset/empty. Only github.com is supported. The token needs repository/PR, checks/status, branch protection read access and GraphQL review-thread access. The token also needs access to effective branch rules (`GET /repos/{owner}/{repo}/rules/branches/{base}`), including organization rulesets. Unavailable or incomplete policies fail closed. A 404 on classic required-status-checks is accepted as no classic status policy, but effective rules must still be readable.
- Forgejo: `JAWA_FORGEJO_TOKEN`. The HTTPS API origin is inferred exclusively from the project's configured repository, using `/api/v1`. Repository administrators must configure a trusted host; never configure an untrusted repository origin. Nonstandard HTTPS ports are supported. SSH repository URLs are accepted as identity inputs, but an SSH port is not automatically translated into an HTTPS port.
- `JAWA_ALLOW_NO_CI=true` explicitly permits GitHub readiness without any checks/statuses, only when there are no required CI contexts/checks. Default: blocked with an explanation. This does not excuse pending/failing CI or unavailable policy evidence.

Tokens stay in process environment/request headers only, never tasks, database records, response diagnostics, or browser state. Redirects are prohibited, client cookies disabled, requests context-bound and capped at 20 seconds per verification. Response bodies are bounded. Provider-supplied pagination URLs are never followed; incomplete/oversized evidence blocks verification rather than silently passing. A supplied HTTP client's transport is trusted infrastructure and must not log credentials or reroute them to untrusted services.

## Immutable task/branch prerequisite

`Attempt.TaskJSON` is BONNIE's actual task object (`task_id`, `text`, etc.), not a repository/branch struct. The verifier checks its task identity and trailing workflow repository/base/provider fields against the card's current project in SQLite. A project configuration change therefore blocks old attempts.

The immutable prompt must end with this additional, unique line:

```
Branch: jawa/<title-slug>-<first-6-card-id-chars>-a<attempt-number>
```

The PR must use exactly that deterministic branch in the configured repository (fork heads are blocked), target the configured base, and match the reported PR number and host/path. Legacy tasks without this line block with `task lacks deterministic Branch instruction`. Existing `jawa/card/<full-card-id>/attempt/<number>` assignments are still accepted. The assigned branch is read from immutable TaskJSON, not recalculated from the current title. No persisted task is rewritten. The workflow emits this branch instruction for each new attempt.

## Readiness policy

**Done means PR-ready for human review/merge, never automatic merge.** This verifier performs no merge, approval, comment-resolution, or provider-policy mutation. Approvals may still be pending, including when branch protection or a `pull_request` ruleset requires approvals. Humans remain responsible for satisfying those merge gates. Pending/unsubmitted reviews and review requests alone are not blockers; outstanding formal changes requests and unresolved inline feedback are.

Both providers require an open, non-draft, unmerged, explicitly mergeable PR with the expected identity and current head SHA. A final PR read catches head changes during verification. Provider state can still change after the last read; verification is not an atomic provider-side snapshot.

### GitHub

Required contexts and app-specific checks are the union of classic branch protection and effective `required_status_checks` rules from repository/organization rulesets. Empty/missing policy fields (including HTTP 200 `{}`) block rather than silently removing requirements. `pull_request` rules are deliberately not enforced as approval requirements under this PR-ready policy; other merge gates also remain human responsibilities.

Required checks must succeed on the current SHA; unrelated optional failures/pending checks do not block when required checks are configured. Without required checks, all reported CI must succeed. Neutral/skipped required checks are conservatively blocked. No CI blocks by default; the existing explicit GitHub-only `JAWA_ALLOW_NO_CI=true` opt-in does not bypass required checks.

REST reviews block outstanding `CHANGES_REQUESTED`, even after a new head; later approval/dismissal from that reviewer clears it (a comment does not). GraphQL independently requires every review thread resolved on the same head SHA. Approvals are not required.

### Forgejo supported policy

The API shape was checked against the public [Forgejo Swagger](https://codeberg.org/swagger.v1.json) and Forgejo's `modules/structs/pull_review.go` / `services/convert/pull_review.go`. The adapter uses:

- `GET /repos/{owner}/{repo}/branches/{base}`: effective `enable_status_check` and `status_check_contexts`, including wildcard branch protection. Missing capability fields block; no guessing from a literal-name protection lookup.
- `GET /repos/{owner}/{repo}/commits/{sha}/status`: combined current-SHA statuses, whose individual state field is **`status`**, not GitHub's `state`. All required contexts must succeed; with no required contexts all CI must succeed. No-CI readiness is not enabled for Forgejo.
- `GET /repos/{owner}/{repo}/pulls/{index}/reviews`: every undismissed `REQUEST_CHANGES` blocks, including stale requests. Explicit dismissal clears that formal request; a newer approval is not assumed to dismiss an older request. Approval counts, pending reviews and `REQUEST_REVIEW` do not gate readiness.
- `GET /repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments`: every inline comment must have a positive `resolver.id`, the documented resolution signal. Review dismissal does **not** resolve comments. Older versions without this signal block only when inline comments exist; empty reviews/comments can pass. Count mismatches and unavailable endpoints block with an explicit reason. Ordinary issue discussion is not treated as an inline review thread.

This is a finite REST policy, not a claim that every Forgejo version or merge gate is supported. Required context patterns are treated as literal names; configurations depending on glob matching may conservatively block. Installations omitting effective branch-policy fields or comment resolver evidence need API support before those cases can pass. Unresolved feedback is never inferred resolved merely from a new commit or a dismissed review.

## Tests

Run `go test ./internal/orchestrator` (or `go test ./...`). Local HTTP/TLS fixtures cover successful GitHub and Forgejo readiness, no-feedback/resolved-feedback Forgejo PRs, required/pending/failing/missing CI, effective rulesets (including approval rules that do not gate readiness), incomplete policy responses, optional CI failures, unresolved/unknown-resolution comments, formal changes requests/dismissal, head races, credentials, malformed responses, cancellation, redirect/pagination/body limits. No live credentials are used.
