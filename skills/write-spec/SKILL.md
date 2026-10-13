---
name: write-spec
description: "Investigate a repository read-only and write an implementation-ready specification for proposeSpec. Use for orchestrator planning tasks; not for implementing changes."
---

# Write an implementation spec

Planning is read-only: do not modify files, commit, push, open PRs, or run
repository scripts or tests. Reading files, `git log`/`git show`/`git grep` and
host read APIs are fine.

## Investigate

1. Pin the commit: `git rev-parse <remote>/<base>` (or the pinned commit in the
   task). Inspect at that commit; you will pass the full SHA to `proposeSpec`.
2. Read `AGENTS.md`, README, architecture docs, and the code paths the card
   touches. Trace callers and tests. Check recent history for related work.
3. Use `clarify` only for genuine product ambiguities the repository cannot
   answer. Batch questions; offer concrete options with your recommendation.

## Spec template

```markdown
## Summary
One paragraph: what will change and why.

## Findings
Current behavior with `path:line` references and evidence.

## Scope
- In scope: …
- Non-goals: …

## Implementation steps
1. Concrete, ordered changes per file/component.

## Testing and acceptance criteria
- Tests to add/update, commands to run, observable acceptance criteria.

## Risks and compatibility
Migrations, API/behavior changes, rollout, open questions.
```

Be specific enough that a separate build run can implement it without
re-investigating. Do not pad; omit empty sections only if truly not applicable.

## Report

Call `proposeSpec(spec, repo_commit)`. On rejection, revise in the same
conversation and propose again. On approval, end with a brief
acknowledgement. If investigation cannot succeed, call `reportBlocker`.
