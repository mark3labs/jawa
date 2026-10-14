# Provider verification

The production orchestrator installs `workflow.SetVerifier(ProviderVerifier(store, nil))` at startup; nil uses the default HTTP transport. The existing workflow polls blocked completed attempts, including after restart; the adapter fetches fresh evidence on every invocation and never caches readiness. Ordinary readiness is historical; optional auto-merge independently revalidates Done candidates.

## Configuration and credentials

- GitHub: `JAWA_GITHUB_TOKEN`, falling back to `GITHUB_TOKEN` when unset/empty. Only github.com is supported. The token needs repository/PR, checks/status, branch protection read access and GraphQL review-thread access. The token also needs access to effective branch rules (`GET /repos/{owner}/{repo}/rules/branches/{base}`), including organization rulesets. Unavailable or incomplete policies fail closed. A 404 on classic required-status-checks is accepted as no classic status policy, but effective rules must still be readable.
- Forgejo: `JAWA_FORGEJO_TOKEN`, falling back to `FORGEJO_TOKEN` when unset/empty. The HTTPS API origin is inferred exclusively from the project's configured repository, using `/api/v1`. Repository administrators must configure a trusted host; never configure an untrusted repository origin. Nonstandard HTTPS ports are supported. HTTP and deployment-subpath repositories are rejected at project creation. Existing unsupported projects must be reconfigured to a root-path HTTPS instance before provider operations can work. SSH repository URLs are accepted as identity inputs, but an SSH port is not automatically translated into an HTTPS port.
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

**Done means PR-ready for human review/merge, not approval of every merge gate.** This verifier performs no merge, approval, comment-resolution, or provider-policy mutation. Approvals may still be pending, including when branch protection or a `pull_request` ruleset requires approvals. Humans remain responsible for satisfying those merge gates. Pending/unsubmitted reviews and review requests alone are not blockers; outstanding formal changes requests and unresolved inline feedback are.

Open PRs must be non-draft and explicitly mergeable, with expected identity and current head SHA. A provider-confirmed closed and merged PR is terminal Done after the same repository/branch/base/head identity checks and final reread; open-PR CI/review gates no longer apply. Closed unmerged PRs fail. A final PR read catches head changes during verification. Provider state can still change after the last read; verification is not an atomic provider-side snapshot.

### GitHub

Required contexts and app-specific checks are the union of classic branch protection and effective `required_status_checks` rules from repository/organization rulesets. Empty/missing policy fields (including HTTP 200 `{}`) block rather than silently removing requirements. `pull_request` rules are deliberately not enforced as approval requirements under this PR-ready policy; other merge gates also remain human responsibilities.

Required checks must have acceptable evidence on the current SHA: completed GitHub check runs with success or skipped conclusions are accepted, including app-specific requirements. Unrelated optional failures/pending checks do not block when required checks are configured. Without required checks, all reported CI must be acceptable; skipped-only check runs count as CI evidence without the no-CI opt-in. Neutral and other non-success/non-skipped conclusions remain blocked, as do non-completed runs. Commit statuses still require success. No CI blocks by default; the existing explicit GitHub-only `JAWA_ALLOW_NO_CI=true` opt-in does not bypass required checks.

REST reviews block outstanding `CHANGES_REQUESTED`, even after a new head; later approval/dismissal from that reviewer clears it (a comment does not). GraphQL independently requires every review thread resolved on the same head SHA. Approvals are not required.

### Forgejo supported policy

The API shape was checked against the public [Forgejo Swagger](https://codeberg.org/swagger.v1.json) and Forgejo's `modules/structs/pull_review.go` / `services/convert/pull_review.go`. The adapter uses:

- `GET /repos/{owner}/{repo}/branches/{base}`: effective `enable_status_check` and `status_check_contexts`, including wildcard branch protection. Missing capability fields block; no guessing from a literal-name protection lookup.
- `GET /repos/{owner}/{repo}/commits/{sha}/status`: combined current-SHA statuses, whose individual state field is **`status`**, not GitHub's `state`. All required contexts must succeed; with no required contexts all CI must succeed. No-CI readiness is not enabled for Forgejo.
- `GET /repos/{owner}/{repo}/pulls/{index}/reviews`: every undismissed `REQUEST_CHANGES` blocks, including stale requests. Explicit dismissal clears that formal request; a newer approval is not assumed to dismiss an older request. Approval counts, pending reviews and `REQUEST_REVIEW` do not gate readiness.
- `GET /repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments`: every inline comment must have a positive `resolver.id`, the documented resolution signal. Review dismissal does **not** resolve comments. Older versions without this signal block only when inline comments exist; empty reviews/comments can pass. Count mismatches and unavailable endpoints block with an explicit reason. Ordinary issue discussion is not treated as an inline review thread.

This is a finite REST policy, not a claim that every Forgejo version or merge gate is supported. Required context patterns are treated as literal names; configurations depending on glob matching may conservatively block. Installations omitting effective branch-policy fields or comment resolver evidence need API support before those cases can pass. Unresolved feedback is never inferred resolved merely from a new commit or a dismissed review.

## Tests

Run `go test ./internal/orchestrator` (or `go test ./...`). Local HTTP/TLS fixtures cover successful GitHub and Forgejo readiness, no-feedback/resolved-feedback Forgejo PRs, required/pending/failing/missing CI, completed skipped check runs (including required/app-specific and skipped-only evidence), effective rulesets (including approval rules that do not gate readiness), incomplete policy responses, optional CI failures, unresolved/unknown-resolution comments, formal changes requests/dismissal, head races, credentials, malformed responses, cancellation, redirect/pagination/body limits. No live credentials are used.

### Terminal merge display detection

The Ready PR display checker confirms explicit `merged=true`, `state=closed`
responses with at most two bounded PR reads. It validates the PR number, base
repository/ref, assigned head ref and head SHA. For terminal display only,
missing/deleted head-repository metadata is allowed; a supplied mismatching
repository is rejected. Base SHA and mergeability may be unavailable after
closure. Initially merged responses require confirmation with the same head
SHA; a clear/conflict reread that has become merged uses the new evidence.
Open PR conflict checks and `ProviderVerifier` readiness identity rules remain
unchanged. Manual merged markers are independent local metadata, not provider
evidence or readiness approval; refresh never clears an applicable marker.

## Optional New card issue writes

New card defaults Create issue on, but creates only on submit, not on selection
or cancellation. GitHub requires repository Issues read/write; Forgejo requires
repository issue-write on its supported version. Read-only credentials remain
sufficient for listing, verification and issue-off cards. Creation uses the same
server token precedence, trusted HTTPS API coordinates, no redirects/cookies,
a 20-second bound and 4 MiB response limit. HTTP/subpath Forgejo is unsupported.
POST contains only title/body and has no internal retry. Valid 201 evidence is
linked using configured coordinates, never returned html_url. Transport failures,
5xx, redirects and malformed success are uncertain: check the repository and
explicitly link/unlink rather than blindly retry. There is no cross-provider
exactly-once guarantee after a lost response; no background outbox retries issues.

## Optional orchestrator auto-merge

Dashboard-only global `auto-merge` defaults false. Cards use live `inherit|on|off`;
explicit On overrides global Off. Existing eligible Ready cards are included.
Force-ready/manual-merged metadata does not authorize writes. Squash only:
GitHub `PUT /repos/{owner}/{repo}/pulls/{number}/merge` includes `sha` and
`merge_method=squash`; Forgejo `POST .../pulls/{index}/merge` requires the supported
`Do=squash` and `head_commit_id` API contract, with `force_merge`,
`merge_when_checks_succeed` and `delete_branch_after_merge` false. Deploy only
Forgejo versions that enforce this expected-head contract; unsupported capabilities
must refuse the request. No administrative bypass, merge queue, branch deletion,
approval/comment resolution or deferred provider auto-merge is requested.

GitHub needs repository **Contents write** plus existing verification read access.
Forgejo needs supported repository merge/write rights plus verification access.
Read-only deployments remain supported with merging disabled. Use credentials
without protection-bypass authority: administrator/bypass tokens can weaken the
provider's final gate for required approvals and policies.

Preparation, one write and confirmation each have a 20-second deadline within a
30-second worker budget. Redirects/cookies are disabled; bodies are capped at
4 MiB. Success is independently confirmed by matching provider PR/head closure.
Uncertain delivery is recorded and read before any retry. Confirmed-open outcomes
may retry only after fresh readiness; unavailable/mismatched evidence remains
uncertain. Backoff starts at one minute, doubles to 30 minutes and honors longer
valid Retry-After. Failures leave historical Ready untouched. Disabling during
flight cannot guarantee stopping an external merge.
