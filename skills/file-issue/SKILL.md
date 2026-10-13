---
name: file-issue
description: "File a bug report, feature request, documentation issue or question on the repository's Git host using its templates, with duplicate checks. Use when the task asks to report, propose or track something rather than implement it."
---

# File an issue

Creating an issue requires task authorization. Follow the system instructions'
Git (host detection and CLI use) and "Working without a human" rules. Without
suitable access, prepare a draft and report the blocker.

## Steps

1. Identify the intended repository and host from the task and
   `git remote -v`, including forks and self-hosted Forgejo.
2. Read contribution rules and issue templates (`.github/ISSUE_TEMPLATE/`,
   `.forgejo/ISSUE_TEMPLATE/`, `.gitea/ISSUE_TEMPLATE/`,
   `.gitlab/issue_templates/`). Respect external trackers and disabled issue
   types. Do not assume template names; translate YAML form fields into body
   headings if the CLI cannot submit forms.
3. Search existing issues (open and closed) for duplicates. If a likely
   duplicate exists, report it (and optionally comment there if authorized)
   instead of filing another.
4. Classify and gather content from the task and repository evidence:
   - Bug: reproduction steps, expected vs actual behavior, version,
     environment, relevant logs or code.
   - Feature: the problem/motivation separately from the proposed solution,
     expected behavior, examples, edge cases, compatibility concerns, optional
     implementation notes.
   - Documentation: file or URL, what is wrong or missing, suggested fix.
   - Question: context and what has been tried.
   If essential information is missing, report what is needed.
5. Fill the template's actual fields; otherwise use the sections above. Follow
   project title conventions, or use a concise `fix:`, `feat:` or `docs:` title.
   Keep observations separate from unverified explanations. Strip credentials
   and private data from logs.
6. Write the body to a unique temporary file and create the issue with the
   verified CLI/API. A template flag alone does not fill required fields. Apply
   only existing, relevant labels.
7. Report the issue number and URL, template or fallback used, and open
   questions. Do not implement or close the issue.
