---
name: create-pr
description: "Create a pull request or merge request for the current branch using actual commits, diffs, and repository templates. Use when the user asks to open a PR or MR on GitHub, Forgejo/Gitea, or GitLab; do not merge it."
---

# Create a pull or merge request

## Task execution

Derive scope, inputs, constraints, and authorization from the spawning task and repository context; this skill has no argument placeholders or interactive input step. Use repository evidence to resolve routine details. If essential information is unavailable or a decision cannot be made safely, stop and report the blocker and required input rather than relying on a follow-up conversation.

Follow repository instructions. Authorization in the spawning task is sufficient for the operations it explicitly covers; do not request it again. Skill activation alone does not authorize committing, pushing, creating remote issues or requests, posting replies, or publishing. Run follow-up skills only when they are within the task's authorized scope. Where the workflow says to ask or wait for approval, proceed only if the task already provides the required decision or approval; otherwise report the blocked state.

## Select the host and repository

Read repository instructions and `git remote -v`. Identify the source remote, target repository, host, and authenticated account; account for forks and self-hosted domains. Do not assume `origin`, GitHub, or `master`. For GitHub use `gh`; for Forgejo/Gitea use an installed compatible CLI such as `tea` or `fj`, or the instance's documented API; for GitLab use `glab`. Check the installed CLI's help and authentication before using it. Never guess flags or assume a Forgejo instance supports GitHub GraphQL. Use existing secure credentials without printing them. If the host or destination is ambiguous, ask. If no suitable authenticated tool is available, provide a draft and explain the blocker instead of claiming creation succeeded.

## Steps

1. Check the branch and working tree. If uncommitted changes exist, stop and suggest the `commit-push` skill. Do not open a request from the target branch itself.
2. Determine the target/default branch from host metadata, repository guidance, or the remote HEAD. Ask if unresolved. Identify the branch's upstream and push any missing commits to the correct source remote without force-pushing.
3. Check for an existing request for this source and target. Report it rather than creating a duplicate.
4. Read the commits and full three-dot diff against the target branch. If the target ref is missing or stale, fetch it from the target remote. Identify linked issues only from evidence. Ask about unrelated changes. Determine whether the actual changes affect user-visible UI (such as layout, styling, components, or visible interactive states), not just whether filenames look UI-related.
5. Find the applicable request template in the repository (for example `.github/`, `.forgejo/`, `.gitea/`, `.gitlab/merge_request_templates/`, or `docs/`). Follow template selection instructions. If absent, use Description, Validation, and Compatibility sections.
6. Draft an accurate title and body. Describe what changed and why. For UI work, follow the UI screenshot evidence guidance below and include a Screenshots / UI evidence section, or use the template's equivalent section. No screenshot work or evidence section is needed for changes without a user-visible UI effect. Check only checklist items that are true. Include issue-closing syntax only when supported by this host and when the change actually resolves that issue.
7. Save the body to a unique temporary file. Create the request with the verified CLI or API, setting the correct target branch and source repository/branch. Use a body file when supported; otherwise encode the file safely as the tool requires.
8. Report the returned URL, target, source, and checks performed, plus attached visual evidence or its outstanding limitation for UI work. Do not merge the request.

## UI screenshot evidence

For user-visible UI work, encourage screenshots before creating the request when tools and task authorization permit. They supplement, not replace, relevant tests and honest validation results; missing screenshots alone should not block an otherwise authorized request. This guidance does not authorize deployments, public artifact uploads, extra credentials, or unrelated changes, and does not require committing screenshots to the repository.

- Run or inspect the implemented UI in a safe local or preview environment and capture actual rendered screenshots of the changed version. Prefer a rendering-capable browser for visual claims. Do not present a text/layout-only tool render as proof of final CSS, fonts, colors, or images; disclose those limitations if that is the only output available.
- Capture relevant after-state evidence. Include a clearly labelled before/after comparison when useful and feasible, without disturbing unrelated working-tree changes to obtain the baseline. Cover the affected state and relevant viewport(s), including a narrow/mobile view for responsive changes where applicable; exhaustive capture is not required. Add short captions identifying the screen, state, viewport, and what each image demonstrates.
- Use sanitized fixture data where possible and inspect images for credentials, tokens, personal information, or private project data before publishing.
- Embed or link actual images in the request body using the host's supported Markdown or attachment mechanism. Use only verified, authorized host capabilities, not guessed CLI upload flags. Ensure reviewers can access the images: a local filesystem path is not an attachment.
- If running the UI, capturing images, or attaching them is unavailable or outside authorization, record the specific limitation and necessary manual follow-up in the request's validation/evidence section. Do not fabricate images, image URLs, screenshots, or visual verification.
