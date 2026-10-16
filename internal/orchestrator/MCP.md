# Remote MCP API

## Endpoint and authentication

MCP is disabled by default. `--mcp-listen` (environment default
`JAWA_ORCHESTRATOR_MCP_LISTEN`) enables a separate HTTP listener at `/mcp`.
Dashboard `mcp-listen` overrides apply after restart; Reset overrides clears them.
Use an HTTPS reverse proxy; built-in TLS is not supported.

The transport is stateless Streamable HTTP, using mcp-go v1.2.1. No session
affinity, resources, subscriptions or server push are required or provided.
Every request requires `Authorization: Bearer <key>`. Missing, invalid or revoked
keys return 401 with `WWW-Authenticate: Bearer realm="jawa"` and
`invalid or missing API key`. Authentication storage failures return a sanitized
503. Any Origin header, including an empty one, returns 403: browser MCP clients
are unsupported. Other paths return 404.

## Keys

Settings → MCP access creates named personal keys with full access (no scopes,
project restrictions or expiry). Creation shows the secret once in a no-store
response, including native-form fallback. Copy it into your client's secure
configuration. Lost keys cannot be recovered: revoke and create a replacement.
Revoked rows remain for audit; revocation rejects the next request, not work
already in progress. Reset dashboard overrides does not revoke keys.

Secrets are `jawa_` plus 64 random hex characters (256 bits). Only the SHA-256
hash is stored; Settings lists the first 13 characters. Last-used tracking writes
at most once per minute. Names are trimmed and limited to 1–64 characters.

## Visible on cards only

The API explicitly projects the board and card/plan page. It never returns run
transcripts, traces, run chat, planning messages/discussion, prompts, provider
bodies, credentials, task JSON, outcome JSON, turn results or run records.
User-authored card descriptions and proposed specs are visible just as in the UI;
do not put credentials in them.

Project objects contain `id`, `name`, `provider`, `repo`, `base_branch`, `created_at`.
Agent choices contain `id`, `provider`, `model`, `availability`, `selectable`, never
transport addresses or arbitrary labels.

Card summaries contain `id`, `key`, `project_id`, `title`, `lane`, `lane_label`,
`position`, `archived`, `status`, `tone`, `working`, optional `agent`/`pr_url`,
`updated_at`, and `version`. Lanes are Todo, Planning, Building, Fix, Done; labels,
status chips, activity and working indicators use the dashboard helpers.
Full cards additionally contain optional `description`, `issue_url`, `pr`,
`blocker`, `notes`, `planning`, and `auto_merge` (inherit/on/off).

- `pr`: URL, check state/label and optional manual-ready provenance.
- `blocker`: reason and `guidance_queued`; its interaction token is carried only
  in the corresponding action's prefilled args.
- `notes`: automatic resume and card diagnostic text.
- `planning`: state, label, revision, approved revision, spec, questions,
  `can_approve`. No conversation or execution payloads.

`version` is the first 16 hex characters of SHA-256 over the full JSON card with
an empty version. List and get versions match; description/spec changes therefore
also invalidate summary versions. It is a change token, not an authorization token.

## Tools and polling

Read tools have the read-only annotation. Results include structured content and
a JSON text block for older clients. Domain failures use `isError: true`, not
JSON-RPC protocol errors. Unexpected storage errors are sanitized to
“Jawa is temporarily unavailable; try again.”

| Tool | Arguments | Result |
| --- | --- | --- |
| `list_projects` | none | `{projects: [...]}` |
| `list_agents` | none | `{available: bool, agents: [...]}`; discovery failure gives false and an empty list |
| `list_cards` | optional `project_id`, `lane`, `include_archived` (false) | `{cards: [...]}` in board order |
| `get_card` | `card_id` | full card |
| `wait_for_card` | `card_id`, `version`, optional `timeout_seconds` (1–25, default 20) | `{changed: bool, card: ...}` |

Read the card, retain its version, then loop `wait_for_card` with that version.
It returns as soon as the card changes or the timeout expires. Always update the
version from the returned card; `changed:false` is an ordinary polling timeout.
Alternatively poll `get_card`. Lifecycle writes below return immediately after
queuing work or recording a decision; they do not wait for an agent.

## Bounds and logging

Ordinary read calls have a 10-second context timeout; wait calls have a 30-second
outer timeout and poll once per second for up to 25 seconds. Requests honor
cancellation. HTTP uses a 5-second header timeout, 120-second idle timeout,
32 KiB maximum headers and 1 MiB maximum request bodies. There is deliberately
no HTTP ReadTimeout or WriteTimeout, since net/http background reads could
otherwise cancel long-poll handlers mid-call.

Logs never contain key names or secrets. Key creation/revocation logs carry
`api_key_id` only; authentication rejection logs Warn with remote address only.
No IP lockout is applied (a reverse proxy may share one address across clients).
Tool failure logs contain tool names, not raw storage/provider error bodies.

## Lifecycle tools

Every write requires completed NATS setup in dashboard Settings. Until then the
result is `isError:true` with “NATS authentication setup is required. Finish
Settings in the dashboard first.” Read tools remain available.

| Tool | Required arguments | Optional arguments / behavior |
| --- | --- | --- |
| `create_project` | `name`, `idempotency_key` | `provider` (github/forgejo/empty), `repo` URL or `repo_name` owner/name + `forgejo_url`, `base_branch` |
| `create_card` | `project_id`, `title`, `idempotency_key` | `description`, `issue_url`, `auto_merge` (inherit/on/off, default inherit), `create_issue`, `issue_recovery`, `recovery_url` |
| `set_auto_merge` | `card_id`, `mode`, `expected_mode` | mode is inherit/on/off |
| `start_planning` | `card_id` | `agent` |
| `reply_planning` | `card_id`, `reply`, `expected_interaction` | Nonblank reply, at most 65536 characters |
| `approve_plan` | `card_id`, `revision` | Revision at least 1; optional `agent` |
| `retry_planning` | `card_id` | Fresh planning attempt |
| `cancel_planning` | `card_id` | Return to Todo |
| `start_build` | `card_id` | `agent`; Planning requires approve_plan instead |
| `retry_build` | `card_id` | Fresh attempt, or start legacy Building card |
| `resume_failed` | `card_id`, `expected_resume` | Same-run planning/build continuation |
| `guide_blocked` | `card_id`, `message`, `expected_interaction` | Nonblank message, at most 64000 characters |
| `cancel_run` | `card_id` | Request only; external actions cannot be undone |
| `reset_card` | `card_id` | Reset inactive work to Todo, retaining history |
| `move_card` | `card_id`, `position` | Nonnegative position within current lane only |

Optional `agent` is an ID from `list_agents`. Omit it for shared-queue work;
explicit targets are presence-validated and never fall back to another agent.
Workflow calls are bounded to 20 seconds; issue creation to 30 seconds.

Success is `{ok:true, card:..., note?:string}`; creation also includes `replayed`.
Project creation returns `project` instead of `card`. Workflow rejection is an
`isError:true` tool result, not a JSON-RPC error, with text
“Unable to apply request: <workflow error>. Call get_card for the current state
and actions.” Issue submission errors retain the dashboard's user-facing text.
Cancellation's success note reminds clients it is not confirmed until the agent
reports cancellation. Applied-but-unreloadable results tell clients to get_card.

### Actions and concurrency

Full cards always include an `actions` array. Each entry contains `tool` and
`args`: the exact prefilled arguments to send, including every concurrency token.
Do not edit tokens. Add any arguments listed in `needs` (for example `reply`,
`message`, `mode`, or `position`). A `note` documents optional agent targeting.
`unavailable` optionally lists tools with dashboard-derived reasons they cannot
currently be used. Actions are presentation, not authorization: Workflow always
revalidates, and a concurrent change can make an action stale. Refresh on rejection.
Action/token changes alter the card version, waking `wait_for_card`.
PR review/merge, archive, deletion, force operations, editing and run chat are not
exposed by these tools.

### Idempotency and issue recovery

Creation requires a key of 1–128 characters from `[A-Za-z0-9._:-]`. Reuse the
same key with identical arguments to retry safely; the key is scoped to the API
key and cannot be reused for another tool/draft. Replays return the original ID
with the **current** resource state. Deleted resources are not recreated. Other
tools rely on Workflow lane/state checks and exact expected-token CAS.

Issue creation uses the same durable submission ledger as the dashboard. It
never blindly retries a provider POST. If the result is uncertain (for example
“Issue creation may have completed…”), check the repository first. Call
`create_card` again with the **same** `idempotency_key`, `project_id`, `title`,
`description`, and `auto_merge`, omitting `create_issue`, and add either:

- `issue_recovery:"link"` with `recovery_url` pointing to the issue; or
- `issue_recovery:"unlinked"` to save without a link.

The ledger rejects changed drafts. To retry a definitely rejected creation, use
a **new** key; the dashboard's explicit retry mode is not exposed. Busy or
interrupted reservations may require restart before recovery, just as in the UI.
Only IDs, tool names and API key IDs are logged for lifecycle actions, never
arguments, titles, replies or provider error bodies.
