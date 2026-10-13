---
name: deliver-pr
description: "End-to-end delivery for orchestrator build and repair tasks: implement on the assigned branch, test, commit, push, open or update the PR, get CI and review bots green, then call completeTask. Use whenever the task authorizes committing, pushing and opening a PR (task text names a Branch and asks to implement a card, approved spec, or repair an existing PR)."
---

# Deliver a pull request

The task text is authoritative for repository, base branch, provider, assigned
branch and what is authorized. Follow the system instructions' Git and
"Working without a human" rules throughout.

## 1. Prepare

1. Read `AGENTS.md`, README, contributing docs, manifests and CI config.
2. Fetch the remote. Check out the assigned branch: track it if it exists on
   the remote, otherwise create it from the latest `<remote>/<base>`.
   Never work on the base branch.
3. Build task: if an open PR already exists for this branch, resume it rather
   than opening another. Read its diff, comments and checks first.
4. Repair task: use the existing PR and its original branch only. Do not create
   a new branch or PR. Merge `<remote>/<base>` into the branch (no rebase, no
   force-push) when the base has moved or conflicts are reported. Resolve
   conflicts preserving both sides' intent, then apply the repair message.

## 2. Implement

1. With an approved spec, implement exactly its scope; note any deviation
   needed and why. Without one, keep to the card's stated scope.
2. Add or update tests. For bugs, reproduce first and add a regression test.
3. Run the project's build, test, lint and format checks as defined by
   instructions and CI. Fix failures your change caused.

## 3. Commit and push

1. Review `git status` and the full diff. Stage explicit paths only.
2. Commit with a Conventional Commits subject (`feat|fix|refactor|docs|test|chore|perf|build(scope): summary`,
   imperative, lowercase, ≤72 chars) and a body explaining what and why.
3. Push to the assigned branch on the correct remote with upstream tracking.
   Never force-push.

## 4. Open or update the PR

Follow the `create-pr` skill for host detection, template, body and UI
evidence, targeting the task's base branch. If a PR for this branch exists,
update its description only when it is now inaccurate.

For new or existing PRs, use `create-pr`'s GitHub screenshot-attachment example
when UI evidence is relevant and posting is authorized; reuse the task's PR
and existing evidence rather than creating a replacement PR or duplicate upload.

## 5. Get to green

Loop until CI and review are clean:

1. Poll CI checks for the current head SHA in ≤540 s sleep chunks. On failure,
   read the logs, reproduce locally where possible, fix, commit, push, repeat.
   Distinguish flaky or infrastructure failures from real ones; re-run once if
   the host supports it, and report evidence rather than guessing.
2. Handle review-bot and actionable review comments with the
   `resolve-reviews` skill, including its rate-limit cooldown procedure. A
   posted cooldown is a wait, not a blocker.
3. Never claim green without checking the current head SHA.

## 6. Report

- Success: call `completeTask` with the PR number once the PR exists, CI for
  the head SHA has passed (or failures are proven unrelated and documented in
  the PR), and no actionable review findings remain.
- Otherwise call `reportBlocker` with a concrete reason: what failed, evidence,
  what was tried, and what decision or access is needed.
- Never call `completeTask` without a real PR number.
