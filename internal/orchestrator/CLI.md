# Orchestrator CLI configuration

Configuration is resolved without changing the process environment:

1. Persisted dashboard settings (when present).
2. Command-line values, including explicit `false` and empty credentials.
3. Environment defaults.
4. Built-in defaults.

Clearing dashboard overrides restores the command/environment defaults. Command
credentials are process-local: they are not written into dashboard settings and
are used for PR verification, conflict checks, and project issue lookup. The UI
reports whether a credential is configured, never its value.

| Flag | Environment default | Built-in default |
| --- | --- | --- |
| `--data-dir` | `JAWA_ORCHESTRATOR_DATA_DIR` | OS config directory + `/jawa` |
| `--listen` | `JAWA_ORCHESTRATOR_LISTEN` | `127.0.0.1:8080` |
| `--nats-listen` | `JAWA_ORCHESTRATOR_NATS_LISTEN` | `127.0.0.1:4222` |
| `--mcp-listen` | `JAWA_ORCHESTRATOR_MCP_LISTEN` | empty (disabled) |
| `--debug` | `JAWA_ORCHESTRATOR_DEBUG` | `false` |
| `--allow-no-ci` | `JAWA_ALLOW_NO_CI` | `false` |
| `--github-token` | `JAWA_GITHUB_TOKEN`, falling back to `GITHUB_TOKEN` | empty |
| `--forgejo-token` | `JAWA_FORGEJO_TOKEN`, falling back to `FORGEJO_TOKEN` | empty |
| `--decision-model` | `JAWA_ORCHESTRATOR_DECISION_MODEL` | empty (no failure triage) |

Boolean environment defaults retain the existing exact `true` convention.
`--allow-no-ci` permits absent GitHub CI evidence; it does not bypass failing or
required checks, provider identity checks, or review gates.

`--decision-model` (for example `openrouter/typesafe/jev-1.14`, with
`OPENROUTER_API_KEY`) enables failure triage before automatic resume; see
[FAILED_RESUME.md](FAILED_RESUME.md#failure-triage). Startup refuses a model Kit
cannot call as a decision model; credentials are checked by the first call.

## Reconcile

`jawa orchestrator reconcile` accepts `--data-dir` from
`JAWA_ORCHESTRATOR_DATA_DIR`, plus the same provider credential and CI-policy flags.
It also accepts:

| Flag | Environment default |
| --- | --- |
| `--nats-url` | `NATS_URL` |
| `--nats-username` | `NATS_USERNAME` |
| `--nats-password` | `NATS_PASSWORD` |

Data directory and NATS URL must be nonempty; reconcile does not infer the
orchestrator's embedded broker address. `--task`, `--run`, and `--attempt` remain
explicit required selections with no environment equivalents. Dashboard provider
settings continue to override command-local values.

Credential defaults and NATS URL defaults (which may include URL credentials) are
omitted from help. Connection errors do not echo URLs or broker error details.
Prefer environment credentials over CLI secrets: command arguments may be visible
in shell history and process listings.

Only runtime configuration is covered. Frontend smoke-test/browser environment
variables and `JAWA_CARD_VISUAL_HTML` are test tooling, not server CLI settings.
The orchestrator does not consume a model environment variable: model selection
belongs to the agent/root command, outside this change's scope.

## Auto merge

Auto merge is dashboard-only: no CLI/environment override. Global default Off is
live for inherited cards; explicit card On overrides Off. Reset dashboard overrides
restores global Off without resetting card preferences. See PROVIDERS.md and README
for squash-only permissions, eligibility and in-flight-disable limitations.
