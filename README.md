# Jawa

A BONNIE v0.14.0 coding agent that receives asynchronous tasks over NATS.
BONNIE supplies sandboxed coding tools; built-in `ask_human` and
`request_approval` tools are disabled with `WithoutHumanInput`. Missing
requirements are reported in the final response rather than suspending the run.
`instructions.md` defines the coding workflow. No custom tool is required.

## Run locally

Requirements: Go 1.27+, BONNIE v0.14.0, a NATS server, and credentials for your
chosen model provider configured through KIT/BONNIE. The default model is
`opencode/glm-5.3-flash`; set `JAWA_MODEL` to override it.

Start a local JetStream broker (`nats-server -js`), then start the agent:

```sh
export NATS_URL=nats://127.0.0.1:4222
export JAWA_NATS_WORKER_ID=jawa-1
export JAWA_NATS_CREATE_STREAM=true # local provisioning only
bonnie dev
# Or build and run a standalone binary:
bonnie build --output ./bin/jawa
./bin/jawa
```

Outside Docker the HTTP API defaults to loopback at `127.0.0.1:8080`.
`JAWA_HTTP_ADDR` overrides the listener; the Docker image sets `0.0.0.0:8080`. Do not expose it without
an authenticator or an authenticated proxy. NATS must be reachable at startup.

With the NATS CLI, subscribe **before** submitting a task:

```sh
nats --server "$NATS_URL" sub bonnie.tasks.results
# In another terminal (with NATS_URL exported):
nats --server "$NATS_URL" pub bonnie.tasks.tasks \
  '{"version":1,"task_id":"coding-001","text":"Write a Go function that reverses a string by rune and test it."}'
```

Results include `task_id`, `run_id`, `state`, and `response` or `error`.
Use a new task ID for each request. Built-in human-input tools are disabled,
so normal tasks do not suspend for clarification. Use BONNIE's typed NATS client
for answering waiting runs; answers are worker-routed in JetStream mode.

Each run has its own sandbox, not access to the host checkout. Seed shared
starter files under `workspace/` **before building**, or provide a repository
URL in the task and ask the agent to clone it. Task `context` is an optional
array of strings, not a host-filesystem mount. Generated files remain in the
run workspace; results contain the agent's summary, not automatic commits or
uploaded artifacts. BONNIE journals run state under `.bonnie/` by default.

## Configuration

| Variable | Default / purpose |
| --- | --- |
| `JAWA_MODEL` | `opencode/glm-5.3-flash` |
| `NATS_URL` | `nats://127.0.0.1:4222` |
| `JAWA_NATS_ROOT_SUBJECT` | `bonnie.tasks`: derives all protocol subjects and enables JetStream |
| `JAWA_NATS_WORKER_ID` | Required: unique, stable worker token |
| `JAWA_NATS_CONSUMER` | Optional shared durable task consumer |
| `JAWA_NATS_CREATE_STREAM` | `false`; exactly `true` permits stream creation |

The simplified root setup derives `bonnie.tasks.tasks`, `.results`, `.events`,
`.answers`, `.commands`, and `.queries`. Answers/commands/queries append the
worker ID for routing. All routes remain under `bonnie.tasks.>`.
**Migration:** the task subject was `bonnie.tasks.requests`; publishers must
now use `bonnie.tasks.tasks` and protocol version 1. The old individual subject
and `JAWA_NATS_STREAM` environment settings are no longer used.

RootSubject always enables JetStream. Permissions limited to `bonnie.tasks.>`
and `_INBOX.>` are no longer sufficient: grant scoped JetStream API, consumer,
and acknowledgement permissions and ensure JetStream is available. A stable
worker ID and provisioned streams are required. Do not deploy this change with
the old restricted account until its permissions have been updated.

For broker authentication select **one** method: `NATS_NKEY_SEED` (user seed
value, not file path), `NATS_TOKEN`, or `NATS_USERNAME`/`NATS_PASSWORD`.
Do not combine those methods or URL credentials. Use TLS and broker permissions
in deployments. Secrets are loaded by the channel, not injected into the agent
sandbox. Do not store them in source or task payloads.

## Durable NATS setup

BONNIE derives stable input, result, and event stream names from the root. By
default the agent binds provisioned streams and never changes existing resources.
Set `JAWA_NATS_CREATE_STREAM=true` only if the account is authorized to provision
them; with a root this permits creation of all three streams. Existing streams
must cover the derived protocol subjects with limits retention. Consult BONNIE's
NATS documentation for provisioning and consumer permissions.

Use `github.com/mark3labs/bonnie/client/nats` with `RootSubject: "bonnie.tasks"`
for durable submission, result/status consumption, status queries, cancellation,
and routing answers to the original worker. Raw tasks must include `"version":1`.
Persist each worker's own journal and sandbox storage; do not share them between
workers. Delivery is at least once, not exactly once: external effects and result
handlers must be safe to repeat. Lost worker state cannot be reconstructed from
the broker alone.

The default Landlock sandbox confines filesystem access but not network
traffic. For untrusted projects, consider BONNIE's Docker sandbox and explicit
network policies. Do not give the agent deployment credentials unnecessarily.

## Docker

The image includes Go 1.27.0, `gh`, Git, build tools, ripgrep, and Lightpanda
(for headless browsing). System-wide Git configuration sets the default commit
identity to `Jawa <jawa@bonnie>`, including inside per-run sandboxes. Repository
configuration can override it. This is commit metadata only, not a GitHub bot
account or custom avatar; PR authorship still comes from `GITHUB_TOKEN`.
It runs Jawa as a non-root user and persists journals
and run workspaces in `/data`. KIT automatically launches `lightpanda mcp` as a
local stdio MCP server and exposes its advertised browser tools to the agent.
No separate browser service, CDP port, or MCP URL is needed. Outside Docker,
install `lightpanda` on PATH before running Jawa. The explicit MCP configuration
in `main.go` replaces MCP servers from KIT config files.

MCP subprocesses are managed by KIT in the agent process environment, not by
BONNIE's per-run shell sandbox. Treat Lightpanda as trusted software with the
container's access; do not assume browser tools have Landlock confinement.

```sh
docker build -t jawa .
# Export these in your shell or inject them from your secret manager first.
# NATS_URL must be reachable from the container (localhost refers to the container).
docker run --rm --init --name jawa \
  -v jawa-data:/data \
  -e NATS_URL -e NATS_USERNAME -e NATS_PASSWORD \
  -e JAWA_NATS_WORKER_ID -e JAWA_NATS_CREATE_STREAM \
  -e GITHUB_TOKEN -e JAWA_MODEL \
  -e OPENCODE_API_KEY \
  jawa
```

Replace `OPENCODE_API_KEY` with the environment variable required by your model
provider if using another provider. All existing `JAWA_NATS_*` settings can also
be passed with `-e`. Do not bake credentials into the image or supply them as
build arguments. No HTTP port is published in this example: NATS is the remote
interface. To use `bonnie chat` from the host, recreate the container with
`-p 127.0.0.1:8080:8080`. The image listens on all container interfaces, but
publishing on host loopback keeps the unauthenticated API local. Avoid
`-p 8080:8080`, which exposes it on all host interfaces. Containers sharing its
network can still reach the API; use trusted networks or add authentication.

`GITHUB_TOKEN` is explicitly passed into sandboxed coding commands so `gh` can
authenticate without `gh auth login`. **Commands can read this token**: use a
least-privilege, repository-scoped token and only trusted tasks. NATS credentials
and model keys are not passed into the coding environment. Never print tokens.
For Git operations over HTTPS, run `gh auth setup-git` in the run workspace
before cloning private repositories. Writes, pushes, and PR creation still
require authorization in the task.

The default Landlock sandbox requires a Linux host with Landlock enabled and a
container security profile that permits Landlock syscalls. No privileged mode
or Docker socket is required or recommended. If your runtime blocks those
syscalls, use a reviewed seccomp profile permitting them rather than disabling
all isolation. Preserve the `jawa-data` volume for waiting/resumed runs.

Lightpanda currently publishes its Linux binaries under `nightly`, so that
default is mutable. For reproducible deployments use an available immutable
release and/or verify the downloaded binary with its SHA-256:

```sh
docker build -t jawa \
  --build-arg LIGHTPANDA_VERSION=nightly \
  --build-arg LIGHTPANDA_SHA256=YOUR_VERIFIED_SHA256 .
```

The image supports amd64 and arm64; hashes are architecture-specific.
`GO_VERSION` is also a build argument (default `1.27.0`).

## Published Docker images

GitHub Actions builds and pushes a Linux amd64 image to
`ghcr.io/mark3labs/jawa` on every branch or tag push. It can also be triggered
manually from the Actions tab. Tags include the branch name (sanitized), the
Git tag for tag pushes, and `sha-<full-commit-sha>`. Only pushes to `master`
update `latest`.

```sh
docker pull ghcr.io/mark3labs/jawa:latest
```

The workflow uses the built-in `GITHUB_TOKEN` with `packages: write`; no extra
registry credentials are required. Runtime NATS, GitHub, and model credentials
are not supplied to the image build. GHCR packages may initially be private:
set the package visibility to public in GitHub package settings if anonymous
pulls are desired, or authenticate to GHCR for private pulls. The first build
creates the package and attaches repository metadata through image labels.

## Automatic Go diagnostics

`golint.go` uses BONNIE's `CompletionHook` and `CompletionFeedback`. Before
accepting a candidate final response it scans workspace Go modules and batches:

1. `go fix ./...` per module (may rewrite source).
2. `gopls check` for Go files belonging to each module.
3. `golangci-lint run ./...` per module.

All commands use `RunScope.Exec`, sharing the agent's sandbox. Module scans
exclude Git metadata, vendor directories, and common tool caches. Scanning all
modules rather than an in-memory edit list includes shell edits and survives
resume/recovery; it also checks pre-existing code, even on read-only tasks.
Each command has a 20-second timeout; diagnostic feedback is capped at 12 KB.
Nonempty output (including go fix rewrites), missing tools, or failed checks
request another turn. At most two repair turns are allowed; unresolved feedback
then fails the run rather than publishing the rejected response. Checks are
skipped on model failure or human-input suspension. A failed module scan fails
the run. Run checks explicitly before pushing: the completion hook cannot undo
external effects performed during the candidate turn.

The Docker image installs pinned `gopls v0.23.0` and `golangci-lint v2.14.0`.
For local execution install these tools on PATH as well.

## Activity and workspace retention

Live activity is logged to stdout with BONNIE's Info-level activity logger,
including run states, tool names, failures, and final responses. Replayed events
are not logged again. Final response text can contain sensitive data; restrict
access to container logs. Debug payload logging is not enabled.

Automatic workspace cleanup runs at startup and every minute (BONNIE's cadence):
completed workspaces are retained 7 days, failed ones 14 days, and cancelled or
retired ones 3 days from their latest terminal checkpoint. Active, pending, and
waiting runs are preserved. Journals and channel addresses remain; after cleanup,
a later turn on a completed conversation starts with an empty workspace. Export
artifacts or push authorized changes before retention expires. Each instance
must own its own journal/workspace volume; do not share a volume between workers.

## Development

```sh
go test ./...
go vet ./...
bonnie build --output ./bin/jawa
```

Edit `main.go` for runtime options and `instructions.md` for behavior. BONNIE
regenerates `bonnie_gen.go`; do not edit it manually.
