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
- `blocker`: reason and `guidance_queued` only, not interaction tokens.
- `notes`: automatic resume and card diagnostic text.
- `planning`: state, label, revision, approved revision, spec, questions,
  `can_approve`. No conversation or execution payloads.

`version` is the first 16 hex characters of SHA-256 over the full JSON card with
an empty version. List and get versions match; description/spec changes therefore
also invalidate summary versions. It is a change token, not an authorization token.

## Tools and polling

All tools have the read-only annotation. Results include structured content and
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
Alternatively poll `get_card`. There are no write tools in this release.

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
