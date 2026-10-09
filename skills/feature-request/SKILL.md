---
name: feature-request
description: "File a feature-request issue using the repository template, motivation, expected behavior, and duplicate checks. Use when the user wants to propose or track a new capability on the Git host, rather than implement it."
---

# File a feature request

## Task execution

Derive scope, inputs, constraints, and authorization from the spawning task and repository context; this skill has no argument placeholders or interactive input step. Use repository evidence to resolve routine details. If essential information is unavailable or a decision cannot be made safely, stop and report the blocker and required input rather than relying on a follow-up conversation.

Follow repository instructions. Authorization in the spawning task is sufficient for the operations it explicitly covers; do not request it again. Skill activation alone does not authorize committing, pushing, creating remote issues or requests, posting replies, or publishing. Run follow-up skills only when they are within the task's authorized scope. Where the workflow says to ask or wait for approval, proceed only if the task already provides the required decision or approval; otherwise report the blocked state.

## Steps

1. Read repository instructions and inspect `git remote -v`. Determine the intended repository and Git host, including self-hosted domains and forks. Ask if ambiguous.
2. Choose an authenticated tool: `gh` for GitHub, a compatible CLI such as `tea` or `fj` for Forgejo/Gitea, `glab` for GitLab, or the host's documented API. Verify installed commands, flags, and authentication with help/status output. Do not assume Forgejo is GitHub or supports GitHub GraphQL. Never print tokens. If no suitable access is available, prepare a draft and report the blocker.
3. Read the repository's feature templates and contribution rules. Check locations such as `.github/ISSUE_TEMPLATE/`, `.forgejo/ISSUE_TEMPLATE/`, `.gitea/ISSUE_TEMPLATE/`, and `.gitlab/issue_templates/`. Respect disabled issue creation or an external tracker. Do not assume a template named `feature_request` exists. Read YAML form fields and translate them into body headings when the CLI cannot submit the form directly.
4. Search existing issues for the requested capability. If a likely duplicate exists, show it and ask before creating another.
5. Clarify missing motivation or expected behavior. Separate the problem from the proposed solution, and distinguish user requirements from implementation suggestions.
6. Use the actual template. If absent, include Feature Description, Motivation / Use Case, Expected Behavior, and optional Proposed Implementation. Include examples, relevant edge cases, and compatibility concerns. Follow repository title conventions; otherwise use a concise `feat: <summary>` title.
7. Write the completed body to a unique temporary file and create the issue with the verified CLI or API. Use a body-file option if supported, or safely encode the body. Do not assume a template flag fills required fields automatically. Apply only existing, relevant labels.
8. Report the returned issue number and URL, template used (or fallback), and any unanswered questions. Do not implement the feature in this workflow.
