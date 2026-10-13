---
name: create-pr
description: "Create (or reuse) a pull or merge request for the current branch from actual commits, diffs and repository templates, with UI screenshot evidence when relevant. Use when the task authorizes opening a PR/MR on GitHub, Forgejo/Gitea or GitLab; never merges."
---

# Create a pull or merge request

Follow the system instructions' Git (host detection and CLI use) and "Working without a human" rules. If no suitable authenticated tool is available, provide a draft and report the blocker instead of claiming creation succeeded.

## Steps

1. Check the branch and working tree. If uncommitted changes exist: commit and push them with `commit-push` when the task authorizes it, otherwise stop. Never open a request from the target branch itself.
2. Determine the target branch from the task, host metadata, repository guidance, or remote HEAD. Push any missing commits to the correct source remote without force-pushing.
3. Check for an existing request for this source and target. Reuse it rather than creating a duplicate.
4. Read the commits and full three-dot diff against the target branch, fetching the target ref if missing or stale. Identify linked issues only from evidence. Determine whether the actual changes affect user-visible UI (layout, styling, components, visible interactive states), not just whether filenames look UI-related.
5. Find the applicable request template (for example `.github/`, `.forgejo/`, `.gitea/`, `.gitlab/merge_request_templates/`, or `docs/`) and follow its selection instructions. If absent, use Description, Validation and Compatibility sections.
6. Draft an accurate title (Conventional Commits style unless the project differs) and body: what changed and why. For UI work, follow the UI screenshot evidence guidance below and include a Screenshots / UI evidence section (or the template's equivalent); skip it for changes without a user-visible effect. Check only checklist items that are true. Use issue-closing syntax only when the host supports it and the change fully resolves the issue.
7. Save the body to a unique temporary file and create the request with the verified CLI or API, setting target branch and source repository/branch. Use a body file when supported.
8. Report the URL and number, target, source, checks performed, and attached visual evidence or its limitation for UI work. Do not merge.

## UI screenshot evidence

For user-visible UI work, encourage screenshots before creating the request when tools and task authorization permit. They supplement, not replace, relevant tests and honest validation results; missing screenshots alone should not block an otherwise authorized request. This guidance does not authorize deployments, public artifact uploads, extra credentials, or unrelated changes, and does not require committing screenshots to the repository.

- Run or inspect the implemented UI in a safe local or preview environment and capture actual rendered screenshots of the changed version. Use a rendering-capable browser (such as Playwright with Chromium, if the repository or environment provides one) for visual claims. Lightpanda screenshots are rasterized page text with no CSS layout, colors, fonts or images; never attach or describe them as UI screenshots.
- Capture relevant after-state evidence. Include a clearly labelled before/after comparison when useful and feasible, without disturbing unrelated working-tree changes to obtain the baseline. Cover the affected state and relevant viewport(s), including a narrow/mobile view for responsive changes where applicable; exhaustive capture is not required. Add short captions identifying the screen, state, viewport, and what each image demonstrates.
- Use sanitized fixture data where possible and inspect images for credentials, tokens, personal information, or private project data before publishing.
- Embed or link actual images in the request body using the host's supported Markdown or attachment mechanism. Use only verified, authorized host capabilities, not guessed CLI upload flags. Ensure reviewers can access the images: a local filesystem path is not an attachment.
- If running the UI, capturing images, or attaching them is unavailable or outside authorization, record the specific limitation and necessary manual follow-up in the request's validation/evidence section. Do not fabricate images, image URLs, screenshots, or visual verification.
