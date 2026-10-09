---
name: file-issue
description: "File a bug, documentation issue, question, or general issue on the repository Git host using the applicable template and duplicate checks. Use when the user asks to report or track a problem; use feature-request for a dedicated feature proposal."
---

# File an issue

## Task execution

Derive scope, inputs, constraints, and authorization from the spawning task and repository context; this skill has no argument placeholders or interactive input step. Use repository evidence to resolve routine details. If essential information is unavailable or a decision cannot be made safely, stop and report the blocker and required input rather than relying on a follow-up conversation.

Follow repository instructions. Authorization in the spawning task is sufficient for the operations it explicitly covers; do not request it again. Skill activation alone does not authorize committing, pushing, creating remote issues or requests, posting replies, or publishing. Run follow-up skills only when they are within the task's authorized scope. Where the workflow says to ask or wait for approval, proceed only if the task already provides the required decision or approval; otherwise report the blocked state.

## Steps

1. Read repository instructions and `git remote -v`. Identify the intended repository, host, and account, including self-hosted Forgejo and forks. Ask if the destination is ambiguous.
2. Use an authenticated tool appropriate to the host: `gh` for GitHub, an installed compatible CLI such as `tea` or `fj` for Forgejo/Gitea, `glab` for GitLab, or the documented host API. Check local help and authentication; do not invent CLI flags or use GitHub GraphQL on Forgejo. Do not expose tokens. Without suitable access, provide a draft and explain what is needed.
3. Read contribution rules and issue templates, including `.github/ISSUE_TEMPLATE/`, `.forgejo/ISSUE_TEMPLATE/`, `.gitea/ISSUE_TEMPLATE/`, or `.gitlab/issue_templates/` when present. Respect external trackers and disabled issue types. Select an existing template that matches the report; do not assume specific template names.
4. Search existing issues for duplicates. Show likely matches and ask before filing a duplicate.
5. Classify the report as a bug, feature, documentation issue, or question. Ask for essential missing information.
   - Bug: reproduction steps, expected and actual behavior, version, environment, and relevant logs or code.
   - Feature: missing capability, motivation, expected behavior, and optional approach.
   - Documentation: file or URL, what is wrong or missing, and suggested correction.
6. Fill the template's actual fields. Convert YAML form fields to body headings if needed. If no template exists, use the sections appropriate to the classification above. Follow project title conventions, or use a concise `fix:`, `feat:`, or `docs:` title when appropriate. Keep observations separate from unverified explanations. Remove credentials and private data from logs.
7. Save the body to a unique temporary file. Create the issue with the verified CLI/API using a body file if supported, or a safely encoded body. A template flag alone does not guarantee completed fields. Apply only existing, relevant labels.
8. Report the returned number and URL, the template or fallback used, and any limitations. Do not implement or close the issue.
