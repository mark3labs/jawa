---
name: update-docs
description: "Update README, guides, API references, examples, changelog or migration notes to match actual code changes. Use when asked to synchronize documentation with a commit, branch, diff or feature; commit, push or publish only if the task separately authorizes it."
---

# Update documentation

Follow the system instructions' Git (host detection and CLI use) and "Working without a human" rules.

## Steps

1. Read repository instructions and identify the change from the task's commit, branch, diff, or topic. Otherwise inspect status, staged/unstaged changes, and recent commits. For branch changes, identify the correct target remote and branch from guidance, remote HEAD, or host metadata. Do not assume `origin/master`. If the baseline or intended scope is unclear, report what is needed before editing.
2. Read the actual diff and related source. If this is the default branch and there is no clear pending change, report that a range or topic is needed rather than guessing what to document.
3. Inventory the applicable surfaces: README, documentation site, guides, API comments/reference, examples, changelog, configuration reference, and migration notes. Discover the actual layout and tools; do not assume Go, a specific docs framework, or fixed directories.
4. Search for existing descriptions of the affected behavior and APIs. Prefer extending the relevant page over creating new pages. Keep navigation and links consistent. Leave unrelated surfaces alone and explain significant omissions.
5. Describe what changed, why, usage, and compatibility. Use real signatures and supported options. Verify examples against source. Match the existing voice, heading style, and code-fence language; link between pages rather than duplicate text.
6. Run the repository's relevant documentation build, link checker, example tests, API reference checks, or lint as defined by instructions and CI. Do not install dependencies or invoke a guessed build command without checking the project setup. Report unavailable checks and failures.
7. Report changed files, deliberately unchanged surfaces, validation results, and remaining concerns. Suggest the `commit-push` skill when ready. Do not commit, push, or publish documentation unless separately asked.
