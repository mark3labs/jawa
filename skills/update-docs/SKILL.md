---
name: update-docs
description: "Update README, guides, API references, examples, changelog, or migration notes to match actual code changes. Use when the user asks to synchronize documentation with a commit, branch, diff, or feature; do not commit, push, or publish unless separately requested."
---

# Update documentation

## Task execution

Derive scope, inputs, constraints, and authorization from the spawning task and repository context; this skill has no argument placeholders or interactive input step. Use repository evidence to resolve routine details. If essential information is unavailable or a decision cannot be made safely, stop and report the blocker and required input rather than relying on a follow-up conversation.

Follow repository instructions. Authorization in the spawning task is sufficient for the operations it explicitly covers; do not request it again. Skill activation alone does not authorize committing, pushing, creating remote issues or requests, posting replies, or publishing. Run follow-up skills only when they are within the task's authorized scope. Where the workflow says to ask or wait for approval, proceed only if the task already provides the required decision or approval; otherwise report the blocked state.

## Steps

1. Read repository instructions and identify the change from the user's commit, request, branch, or topic. Otherwise inspect status, staged/unstaged changes, and recent commits. For branch changes, identify the correct target remote and branch from guidance, remote HEAD, or host metadata. Do not assume `origin/master`. If the baseline or intended scope is unclear, ask before editing.
2. Read the actual diff and related source. If this is the default branch and there is no clear pending change, ask for a range or topic rather than guessing what to document.
3. Inventory the applicable surfaces: README, documentation site, guides, API comments/reference, examples, changelog, configuration reference, and migration notes. Discover the actual layout and tools; do not assume Go, a specific docs framework, or fixed directories.
4. Search for existing descriptions of the affected behavior and APIs. Prefer extending the relevant page over creating new pages. Keep navigation and links consistent. Leave unrelated surfaces alone and explain significant omissions.
5. Describe what changed, why, usage, and compatibility. Use real signatures and supported options. Verify examples against source. Match the existing voice, heading style, and code-fence language; link between pages rather than duplicate text.
6. Run the repository's relevant documentation build, link checker, example tests, API reference checks, or lint as defined by instructions and CI. Do not install dependencies or invoke a guessed build command without checking the project setup. Report unavailable checks and failures.
7. Report changed files, deliberately unchanged surfaces, validation results, and remaining concerns. Suggest the `commit-push` skill when ready. Do not commit, push, or publish documentation unless separately asked.

For remote request context, detect the actual Git host. Use authenticated GitHub, Forgejo/Gitea, GitLab tooling or the host's documented API as appropriate; verify commands and do not expose credentials. If remote access is unavailable, request the relevant diff or description.
