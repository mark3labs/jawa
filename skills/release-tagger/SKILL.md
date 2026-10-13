---
name: release-tagger
description: "Analyze changes since the previous release and propose a semantic version, annotated tag message, target commit and remote; create and push the tag only when the task pre-approves those exact values. Use when asked to prepare or tag a release."
---

# Prepare a release tag

Follow the system instructions' Git (host detection and CLI use) and "Working without a human" rules.

Creating or pushing a tag requires the task to approve the exact version, target commit and remote. Without that, stop after step 7 and report the proposal.

## Steps

1. Read release instructions, manifests, changelog, CI, and version conventions. Determine whether the project uses semantic versions, a tag prefix, prereleases, component-specific releases, or another scheme. Do not assume a Go project or a `v` prefix. If semantic versioning is inappropriate, report that and stop.
2. Inspect status, current branch, HEAD, and remotes. Identify the release remote and intended commit/branch from the task or project guidance. If ambiguous, report it; do not assume `origin` or the default branch. Stop if there are uncommitted release changes or the release commit is unclear.
3. Fetch tags from the selected remote without overwriting existing tags. Inspect relevant release tags by semantic version, not lexicographic order or tag date alone. Identify the correct previous release for the component/release line; if none exists, treat the first version as a decision the task must supply.
4. Read commits and the full relevant diff since that release. Review public interfaces, behavior, compatibility, and release notes. Commit prefixes are clues, not proof of compatibility.
5. Propose the semantic bump: major for incompatible public changes, minor for backward-compatible functionality, patch for compatible fixes. Apply the project's documented pre-1.0 and prerelease policy. Never choose a smaller bump merely because it appears conservative. If there are no release-worthy changes, suggest skipping.
6. Check required version-file, changelog, packaging, and release validation steps. Report any steps needed before tagging; do not silently modify version files or commit them. Run appropriate existing checks when feasible.
7. Draft the exact tag name, target commit, remote, and annotated message, grouped into Features, Fixes, Breaking Changes, and other relevant sections. Note that pushing a tag may trigger publishing pipelines.
8. Proceed only if the task approves this exact version, target commit and destination; otherwise report the proposal and stop.
9. With approval, verify the target commit still matches and the tag does not already exist locally or remotely. Create an annotated tag, then push only that tag to the approved remote. Do not overwrite tags, force-push, or push all tags. Report the result and any publishing workflow status you actually checked.

Git tags work on GitHub, Forgejo, and other Git hosts. Creating a hosted release is a separate operation and requires explicit task authorization and verified host-specific tooling.
