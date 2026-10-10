# Jawa

![Jawa logo](logo.png)

A coding agent built on [BONNIE](https://github.com/mark3labs/bonnie). Jawa receives
work over NATS, edits code, runs tests, and can push branches and create pull
requests. Lightpanda MCP provides browser tools.

The image includes Go, Git, GitHub CLI, Go linters, Python, and common development
utilities.

## Orchestrator

The same binary can serve a local control plane with SQLite and an embedded,
authenticated, persistent NATS JetStream server:

```sh
go build -o ./bin/jawa .
./bin/jawa orchestrator --data-dir ./jawa-data \
  --listen 127.0.0.1:8080 --nats-listen 127.0.0.1:4222
```

Open http://127.0.0.1:8080 and create an admin username/password. Add projects tied
to GitHub or Forgejo repository URLs, then create cards. The Todo / Building / Done
board supports drag-and-drop and keyboard moves, persistent ordering, and optional
issue links. Open **Agent connection settings** to set NATS credentials before
connecting agents; initial random credentials deliberately cannot be used.

Moving a Todo card to **Building** atomically creates a durable execution attempt
and outbox task. The orchestrator publishes it to `bonnie.tasks`, consumes BONNIE
events/results, and shows agent assignment, PR links, blockers, and attempt history.
Agents appear live through the BONNIE presence API; enable presence on each agent:

```sh
export JAWA_NATS_PRESENCE=true
export JAWA_NATS_AGENT_ID=jawa-1
export NATS_URL=nats://127.0.0.1:4222
export NATS_USERNAME=agent
# Set NATS_PASSWORD to the credential configured in the admin UI.
./bin/jawa
```

The orchestrator independently verifies PR identity, assigned branch, current-head
CI and review feedback before moving a card to **Done**. Done means ready for human
review/merge, not merged or automatically approved. Give the orchestrator
`JAWA_GITHUB_TOKEN` (or `GITHUB_TOKEN`) and/or `JAWA_FORGEJO_TOKEN` with verification
read access; agents need their own push/PR credentials. See [PROVIDERS.md](PROVIDERS.md)
for supported API policy and required permissions. Missing credentials or incomplete
evidence leaves the card blocked in Building rather than falsely completing it.

New branches use `jawa/<title-slug>-<6-char-card-id>-a<attempt>`, for example
`jawa/update-ci-6780b3-a1`. Existing assignments are preserved.

Retry creates another numbered branch/attempt after a terminal failed/blocked
outcome. Legacy Building cards without attempts offer Start work. Reset to Todo
preserves history; Delete requires confirmation. Active attempts cannot be reset/deleted. Active attempts cannot be manually moved out of Building: cancellation may be requested for a pinned active agent run, but the request is not confirmation and cannot undo external actions already taken. Execution leases, automatic reassignment, and enforced runtime/cost budgets are not
yet implemented. The agent prompt bounds CI/review repair to 30 minutes, but this
is guidance, not a scheduler-enforced timeout. Review comments arriving after Done
do not automatically reopen work. Use one orchestrator per database and NATS scope;
its stable durable consumers are not multi-orchestrator isolated.

### Recover agent-side re-executions

Results from an execution whose identity differs from the pinned run are retained
rather than silently discarded. Startup performs a bounded read-only scan of retained
broker results (last 10,000 sequence positions, five seconds). Operator selection is
explicit and audited; it never bypasses provider verification or republishes a task:

```sh
./bin/jawa orchestrator reconcile --data-dir /path/to/jawa-data \
  --task TASK_ID --run RUN_ID --attempt REMOTE_ATTEMPT_ID \
  --nats-url nats://127.0.0.1:4222
```

Credentials come from `NATS_USERNAME`/`NATS_PASSWORD`. Repeated selection can recheck
an already-pinned result. A completed report may be bare JSON or one JSON code fence.
Provider-confirmed merged PRs also count as Done, provided repository, branch, base,
and head identity checks pass. Closed unmerged PRs remain blocked.

The UI separates **Board** (one project at a time), **Runs**, **Agents**, and
**Settings** into bookmarkable views. It uses templ and prebuilt shadcn-templ
components, with a dark Jawa-inspired robe-brown/sand/amber palette, compact
cards, keyboard-accessible action menus, and self-hosted Inter typography.
Datastar SSE morphs only each view's keyed content, sidebar counts, and agent
chip. Rocket (Datastar v1.0.4 + Rocket beta.2) wraps SortableJS, menus, relative
timestamps, clipboard controls, and keyboard shortcuts. No production CDN is used.

Keyboard shortcuts: `c` opens a new card, `/` focuses the board filter, and
`g` followed by `b`, `r`, `a`, or `s` opens Board, Runs, Agents, or Settings.
Shortcuts are disabled while typing or using a dialog. Card actions are in the
ellipsis menu; detailed execution history lives in Runs, not inside board cards.
See [`internal/orchestrator/CONTRACT.md`](internal/orchestrator/CONTRACT.md) for
UI regeneration and browser smoke-test instructions.

Keep HTTP and NATS on loopback unless protected by appropriate TLS and network
controls. There is no native orchestrator TLS configuration yet. Keep the data
directory private and back it up; do not share it with agent workspaces.

## Run with Docker

Configure your environment:

```sh
export NATS_URL=tls://your-nats-server:4222
export NATS_USERNAME=agent
export NATS_PASSWORD='your-password'
export OPENCODE_API_KEY='your-api-key'
export GITHUB_TOKEN='your-github-token'
export JAWA_NATS_AGENT_ID=jawa-1
```

Start Jawa:

```sh
docker run -d --init --name jawa \
  -p 127.0.0.1:8080:8080 \
  -v jawa-data:/data \
  -e NATS_URL -e NATS_USERNAME -e NATS_PASSWORD \
  -e OPENCODE_API_KEY -e GITHUB_TOKEN -e JAWA_NATS_AGENT_ID \
  ghcr.io/mark3labs/jawa:latest
```

Use a JetStream-enabled NATS server. The agent creates the required streams,
so its account needs stream/consumer administration and publish/subscribe
permissions for the `bonnie` protocol routes, agent routes, reply inboxes, and
JetStream API/acknowledgements.

Each instance needs a unique, stable agent ID and its own data volume. Jawa uses
Local sandbox mode: coding commands have the container user's filesystem and
environment access, including credentials. Run trusted tasks and keep the HTTP
port bound to host loopback.

View activity:

```sh
docker logs -f jawa
```

## Send a task

Clone this repository, copy `.env.example` to `.env`, and fill in your NATS
credentials. The sample client loads `.env` from the current directory; existing
environment variables take precedence. `-root` overrides `JAWA_NATS_ROOT_SUBJECT`.
Then run:

```sh
go run ./cmd/submit \
  -text 'Clone mark3labs/kit, run tests and give me a report'
```

Assign work to a specific instance:

```sh
go run ./cmd/submit -agent jawa-1 \
  -text 'Clone OWNER/REPO, implement the requested change, test it, and open a PR. You are authorized to push a branch and create the PR.'
```

The client logs pickup and status changes, then prints the result. Use `-id` to
identify a request and `-timeout` to adjust the wait (default: 30 minutes).
Targeted tasks wait for their assigned agent. Delivery is at least once; make
external effects safe to repeat.

For an existing input stream, ensure its subjects include `bonnie.tasks.agent.*`
before enabling targeted agents. BONNIE validates existing streams rather than
modifying them.

## Chat locally

Install the BONNIE CLI, then connect to the published HTTP port:

```sh
go install github.com/mark3labs/bonnie/cmd/bonnie@v0.20.0
bonnie chat
```

## Configuration

The Jawa binary loads `.env` from its current working directory before reading
configuration. Existing environment variables take precedence. A missing `.env`
is allowed; unreadable or invalid files stop startup without printing their contents.

| Variable | Default / purpose |
| --- | --- |
| `JAWA_NAME` | `jawa`; agent display name (`--name` overrides) |
| `JAWA_MODEL` | `opencode/glm-5.3-flash` |
| `JAWA_NATS_AGENT_ID` | Required, e.g. `jawa-1` (letters, digits, hyphens); `--nats-agent-id` overrides. |
| `JAWA_NATS_ROOT_SUBJECT` | `bonnie` |
| `JAWA_NATS_CREATE_STREAM` | `true` |
| `JAWA_NATS_CONSUMER` | Optional shared task consumer override |
| `JAWA_NATS_PRESENCE` | Set `true` to enable BONNIE JetStream KV agent discovery |
| `JAWA_NATS_PRESENCE_BUCKET` | `jawa_agents`; use the same bucket for a discovery scope |

The compiled binary also accepts serving flags for the agent name and agent ID:

```sh
./bin/jawa --name jawa-dev --nats-agent-id jawa-1 --sandbox microsandbox
```

Flags override environment/`.env` values. The display name does not change NATS
routing; agent IDs must still be unique and stable for each instance.

The root derives `bonnie.tasks`, `bonnie.results`, `bonnie.events`, and the
`bonnie.answers`, `bonnie.commands`, and `bonnie.queries` control routes.

For GitHub pushes and PRs, give `GITHUB_TOKEN` repository **Contents** and
**Pull requests** read/write permissions. Commits default to `Jawa <jawa@bonnie>`.

Run state and workspaces persist under `/data`. Completed workspaces are retained
7 days, failed ones 14 days, and cancelled/retired ones 3 days. Export important
artifacts before retention expires.

## Microsandbox

Jawa registers Local (default) and Microsandbox backends through BONNIE's
`WithSandboxes`. Select the microVM backend with the compiled binary's flag:

```sh
bonnie build --output ./bin/jawa
./bin/jawa --sandbox microsandbox
```

On a Linux/KVM-capable host, BONNIE automatically installs its pinned
microsandbox runtime when `msb` is absent. Existing installations are kept;
upgrade an older installation yourself if needed. Initial installation needs
network access. Each guest uses `ghcr.io/mark3labs/jawa:latest`, 4 GiB RAM,
and 2 CPUs; `JAWA_SANDBOX_IMAGE` can override the image for testing.

Container deployments must provide KVM and the runtime's host prerequisites.
Lightpanda MCP runs alongside the agent; coding commands and completion checks
run in the selected backend. Keep backend state and avoid switching backends
for in-flight runs.

## Develop

Requires Go 1.27+. Edit `instructions.md` for agent behavior and `main.go` for
runtime configuration. Put starter files in `context/`; BONNIE copies them
into new run working directories.

```sh
go test ./...
go vet ./...
go test -race ./...
golangci-lint run ./...
docker build -t jawa:local .
# Requires local Chromium and frontend npm dependencies:
go run ./cmd/factory-smoke
```

Pushes publish images to GHCR with branch and commit tags; `master` updates
`latest`.
