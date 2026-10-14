---
name: deliver-pr
description: "End-to-end delivery for orchestrator build and repair tasks: implement on the assigned branch, test, commit, push, open or update the PR, verify current-head checks and all-author feedback, then call completeTask. Use whenever the task authorizes committing, pushing and opening a PR (task text names a Branch and asks to implement a card, approved spec, or repair an existing PR)."
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
evidence, targeting the task's base branch. If you open the PR before the
implementation is finished, prefix its title with `WIP: ` and remove the
prefix once the work is complete and pushed. If a PR for this branch exists,
update its description only when it is now inaccurate.

For new or existing PRs, use `create-pr`'s GitHub screenshot-attachment example
when UI evidence is relevant and posting is authorized; reuse the task's PR
and existing evidence rather than creating a replacement PR or duplicate upload.

## 5. Get to green

Use the same evidence-driven loop for build and repair tasks:

1. After PR creation and every push, inspect current-head checks: obtain the
   actual PR head SHA, then query host jobs/check runs and commit statuses with
   supported CLI/API operations and pagination. Inspect expected/required
   contexts too; missing required evidence is not success. Neither workflow
   configuration, an older SHA, nor an empty result proves green.
2. Wait for observed pending work (queued/in-progress jobs, including review
   jobs) within the task's applicable time budget. Use ≤540 s sleep chunks and
   recheck between chunks. A review running without a check may be waited on
   only with actual host/bot progress evidence, not assumed scheduling.
3. Inspect failures and logs, reproduce where possible, fix, validate locally,
   commit and push; restart this loop for the new head. Diagnose flaky or
   infrastructure failures with evidence and use authorized host-supported
   reruns where appropriate. Documenting a failure does not make it green.
4. Fetch all-author feedback with pagination: review summaries, inline threads
   and PR discussion comments, including comments posted during checks. Use
   `resolve-reviews` to address actionable findings from humans and bots.
   After fixes or any push, re-fetch all feedback and current-head evidence.
5. Immediately before reporting, re-read the head, checks and comments. If the
   head changes, discard stale check evidence and repeat. Complete only when
   applicable checks succeed or are accepted by existing host policy, no
   observed jobs remain pending, and no actionable or provider-blocking review
   feedback remains. Do not wait for or request a nonexistent review. Absence
   of a posted/running review does not itself justify waiting or solicitation.
6. No checks observed is not green checks. Follow the existing repository/provider
   no-CI policy or report missing evidence; never enable a bypass to finish.
   Unreadable/incomplete evidence and missing required checks cannot be ignored.

## 6. Report

- Success: call `completeTask` with the positive PR number only after the final
  current-head checks and feedback reread satisfies the loop above. Report the
  observed evidence, not an assumed review schedule or integration.
- Otherwise call `reportBlocker` with a concrete reason: what failed, evidence,
  what was tried, and what decision or access is needed. Exceeding the task's
  applicable waiting budget with pending work is not success.
- Never call `completeTask` without a real PR number.
