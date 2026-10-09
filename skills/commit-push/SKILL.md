---
name: commit-push
description: "Review intended working-tree changes, run relevant checks, create a Conventional Commits commit, and push the current branch. Use when the user explicitly asks to commit and push completed work; not for review-only tasks or unauthorized publishing."
---

# Commit and push

## Task execution

Derive scope, inputs, constraints, and authorization from the spawning task and repository context; this skill has no argument placeholders or interactive input step. Use repository evidence to resolve routine details. If essential information is unavailable or a decision cannot be made safely, stop and report the blocker and required input rather than relying on a follow-up conversation.

Follow repository instructions. Authorization in the spawning task is sufficient for the operations it explicitly covers; do not request it again. Skill activation alone does not authorize committing, pushing, creating remote issues or requests, posting replies, or publishing. Run follow-up skills only when they are within the task's authorized scope. Where the workflow says to ask or wait for approval, proceed only if the task already provides the required decision or approval; otherwise report the blocked state.

## Steps

1. Read the repository instructions. Run `git status`, `git diff`, and `git diff --cached`. Read new untracked files too; do not infer changes from filenames alone.
2. Check for secrets, generated output, and unrelated changes before staging. This workflow normally stages all changes with `git add -A`; ask before including anything unexpected. Do not force-add ignored files without permission. If there is nothing to commit, report that and stop.
3. Run the relevant checks specified by the repository. Report failures; do not claim tests passed if they did not run.
4. Write a Conventional Commits message: `<type>(<optional-scope>): <summary>`. Use `feat`, `fix`, `refactor`, `chore`, `docs`, `test`, `perf`, or `build`. Use an imperative, lowercase summary with no trailing period; keep the subject at most 72 characters. Add a body for non-trivial changes to explain what and why. Do not add generated-by text.
5. Commit the reviewed changes. Prefer one logical commit; ask before splitting clearly unrelated work.
6. Check the current branch and upstream. Use `git push` when an upstream exists. Otherwise identify the intended remote from `git remote -v` and repository guidance; ask if ambiguous, then push the branch with upstream tracking. Do not assume the remote is `origin`, and do not force-push.
7. Report the commit hash, subject, push destination, and check results. Stop on a failed command and report the error rather than continuing as if it succeeded.

Git push works with GitHub, Forgejo, and other Git hosts; no platform-specific CLI is required.
