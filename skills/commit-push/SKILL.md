---
name: commit-push
description: "Review working-tree changes, run relevant checks, create a Conventional Commits commit, and push the current branch. Use when the task authorizes committing and pushing; not for review-only tasks. Orchestrator build/repair tasks should follow deliver-pr, which includes this."
---

# Commit and push

Follow the system instructions' Git and "Working without a human" rules.

## Steps

1. Read repository instructions. Run `git status`, `git diff` and
   `git diff --cached`. Read new untracked files; do not judge by filename.
2. Decide what belongs in this commit. Stage explicit paths (`git add <paths>`),
   not `git add -A`. Exclude secrets, build output, caches, screenshots, run
   artifacts and unrelated changes; never force-add ignored files. If something
   unexpected looks like it belongs, treat it as a decision (see system
   instructions). If nothing is left to commit, report that and stop.
3. Refuse to commit on the base/default branch; create or switch to a feature
   branch (or the task's assigned branch) first.
4. Run the relevant checks the repository defines. Report failures; never claim
   checks passed if they did not run.
5. Message: `<type>(<optional-scope>): <summary>` with type `feat`, `fix`,
   `refactor`, `chore`, `docs`, `test`, `perf` or `build`; imperative, lowercase,
   no trailing period, ≤72 chars. Add a body for non-trivial changes (what and
   why). No generated-by text.
6. Prefer one logical commit. Split clearly unrelated work into separate
   commits rather than mixing it.
7. Push: `git push` if an upstream exists; otherwise choose the remote from the
   task, repository guidance or `git remote -v` (do not assume `origin`) and
   `git push -u <remote> <branch>`. Never force-push.
8. Report commit hash, subject, push destination and check results. Stop on a
   failed command and report the error.
