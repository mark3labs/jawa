---
name: code-audit
description: "Perform a read-only repository audit for dead code, meaningful duplication, architecture boundary violations, and incremental refactor options. Use when the user asks for a code health audit or cleanup recommendations without implementing changes."
---

# Read-only code audit

## Task execution

Derive scope, inputs, constraints, and authorization from the spawning task and repository context; this skill has no argument placeholders or interactive input step. Use repository evidence to resolve routine details. If essential information is unavailable or a decision cannot be made safely, stop and report the blocker and required input rather than relying on a follow-up conversation.

Follow repository instructions. Authorization in the spawning task is sufficient for the operations it explicitly covers; do not request it again. Skill activation alone does not authorize committing, pushing, creating remote issues or requests, posting replies, or publishing. Run follow-up skills only when they are within the task's authorized scope. Where the workflow says to ask or wait for approval, proceed only if the task already provides the required decision or approval; otherwise report the blocked state.

Do not edit, rename, delete, stage, or commit files. Do not install tools, update dependencies, or run formatters in write mode. Run analysis only when it does not modify the working tree; place necessary build output and caches outside it.

## Steps

1. Map the repository's languages, packages, modules, applications, and public interfaces. Read applicable `AGENTS.md` files, README, architecture documents, manifests, and CI. Select audit scope from user input; otherwise prioritize central code paths. Skip generated code, vendor trees, and third-party copies.
2. Find dead-code candidates through references, imports, call sites, obsolete files, removal TODOs, and commented-out blocks. Use available project-appropriate static analyzers in read-only mode. Do not assume Go or require an unavailable tool. Public APIs, plugins, reflection, generated entry points, and external consumers can make apparently unused symbols necessary; state confidence and limitations.
3. Find duplication with the same purpose and maintenance needs. Do not flag independent code merely because it looks similar. Identify each site and a possible shared home that respects existing boundaries.
4. Check documented boundaries: dependency direction, public API exposure, UI/business/data separation, cycles, and cross-module coupling. Cite the actual rule or evidence rather than imposing Kit-specific architecture or personal preferences.
5. Identify incremental refactor options: oversized functions, deeply nested conditions, mixed responsibilities, repeated setup, or awkward APIs. Explain the current shape, proposed shape, benefit, and risk. Do not propose sweeping rewrites or purely stylistic changes.
6. Cross-check findings against repository constraints and known runtime/compiler/framework limitations. Drop recommendations that would reintroduce a documented problem.
7. Report in the final message, not a new file:
   - Summary and audit scope, including tools run and unavailable checks.
   - Dead code, grouped by high/medium/low confidence.
   - Duplication clusters and suggested shared homes.
   - Boundary violations, with the relevant rule and fix outline.
   - Refactor options, with benefit and risk.
   - Prioritized next steps.

Cite each finding with `path:line` and supporting evidence. Prefer a small number of useful findings over speculative volume. End by stating that no files were modified and recommend small, separately reviewed follow-up changes.
