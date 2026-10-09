---
name: release-tagger
description: "Analyze changes since the previous release and propose a semantic version, annotated tag message, target commit, and remote. Use when the user asks to prepare or tag a release; wait for explicit approval of the proposal before creating or pushing the tag."
---

# Prepare a release tag

## Task execution

Derive scope, inputs, constraints, and authorization from the spawning task and repository context; this skill has no argument placeholders or interactive input step. Use repository evidence to resolve routine details. If essential information is unavailable or a decision cannot be made safely, stop and report the blocker and required input rather than relying on a follow-up conversation.

Follow repository instructions. Authorization in the spawning task is sufficient for the operations it explicitly covers; do not request it again. Skill activation alone does not authorize committing, pushing, creating remote issues or requests, posting replies, or publishing. Run follow-up skills only when they are within the task's authorized scope. Where the workflow says to ask or wait for approval, proceed only if the task already provides the required decision or approval; otherwise report the blocked state.

## Steps

1. Read release instructions, manifests, changelog, CI, and version conventions. Determine whether the project uses semantic versions, a tag prefix, prereleases, component-specific releases, or another scheme. Do not assume a Go project or a `v` prefix. If semantic versioning is inappropriate, ask how to proceed.
2. Inspect status, current branch, HEAD, and remotes. Identify the release remote and intended commit/branch from project guidance or user input. Ask if ambiguous; do not assume `origin` or the default branch. Stop if there are uncommitted release changes or the release commit is unclear.
3. Fetch tags from the selected remote without overwriting existing tags. Inspect relevant release tags by semantic version, not lexicographic order or tag date alone. Identify the correct previous release for the component/release line; ask about an initial release if none exists.
4. Read commits and the full relevant diff since that release. Review public interfaces, behavior, compatibility, and release notes. Commit prefixes are clues, not proof of compatibility.
5. Propose the semantic bump: major for incompatible public changes, minor for backward-compatible functionality, patch for compatible fixes. Apply the project's documented pre-1.0 and prerelease policy. Never choose a smaller bump merely because it appears conservative. If there are no release-worthy changes, suggest skipping.
6. Check required version-file, changelog, packaging, and release validation steps. Report any steps needed before tagging; do not silently modify version files or commit them. Run appropriate existing checks when feasible.
7. Draft the exact tag name, target commit, remote, and annotated message, grouped into Features, Fixes, Breaking Changes, and other relevant sections. Note that pushing a tag may trigger publishing pipelines.
8. Wait for the user's confirmation of the version, message, target commit, and destination before creating or pushing any tag.
9. After approval, verify the target commit still matches and the tag does not already exist locally or remotely. Create an annotated tag, then push only that tag to the approved remote. Do not overwrite tags, force-push, or push all tags. Report the result and any publishing workflow status you actually checked.

Git tags work on GitHub, Forgejo, and other Git hosts. Creating a hosted release is a separate operation and requires explicit user approval and verified host-specific tooling.
