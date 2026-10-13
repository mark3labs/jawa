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

The orchestrator writes timestamped Charm logs to stderr for startup, task
queueing/dispatch, execution status/results, cancellation, verification, recovery,
and NATS connection lifecycle. Add `--debug` to include routine recovery polling
and ignored stale/duplicate deliveries. Logs contain execution IDs and states,
not prompts, raw result payloads, credentials, or remote error text.

Open http://127.0.0.1:8080 and create an admin username/password. You are then
prompted in **Settings** to configure NATS authentication: the default agent
username is `worker`; choose a password of 12–72 bytes and save to continue.
Until this succeeds, workspace pages return to Settings and project/card actions
are blocked with a setup alert. Completion persists across restarts. Existing
installations without a completion marker must confirm their credentials once;
the initial random bootstrap password does not count as completed setup.

Authentication lasts an absolute **30 days** from account creation or successful
login; activity does not renew it. Refresh and browser/PWA relaunch retain a valid
session when the same origin, browser profile and server data directory are kept.
Logout revokes it immediately. Existing sessions keep their previous expiry:
after upgrading, log in again to receive the longer lifetime. Clearing browser
storage or platform eviction can require another login. Some platforms separate
browser and installed-app cookie stores; log in inside the installed app first.
Never store passwords or bearer tokens in JavaScript storage. See the
[mobile/PWA acceptance checklist](internal/orchestrator/CONTRACT.md#mobile-and-pwa-checks)
for disposable browser automation and physical-device validation.

Add projects by
selecting a provider and entering a repository name (`owner/repo`). GitHub uses
`https://github.com` automatically. For Forgejo, also enter the instance URL;
it defaults to `https://codeberg.org` and supports self-hosted HTTP(S) instances,
including ports and deployment subpaths. Then create cards. The Todo / Planning /
Building / Done board supports drag-and-drop and keyboard moves, persistent ordering, and optional
issue links. Open **Agent connection settings** to set NATS credentials before
connecting agents; initial random credentials deliberately cannot be used.

The **New card** dialog lists open issues from the selected project's repository
as `#number title`; choosing one stores its issue link. Changing projects clears
the previous selection without changing your draft. Selecting an issue alone does
not copy its text. **Autofill** explicitly replaces both Title and Description
with the selected issue’s title and raw body (or an empty description for a
bodyless issue); both fields remain editable. It uses the loaded snapshot.
**Refresh issues** retries loading or obtains a newer snapshot before copying.
You can still create a card with **No linked issue** when the list is empty or
unavailable. The orchestrator needs `JAWA_GITHUB_TOKEN` (or `GITHUB_TOKEN`) with
repository issue read access for GitHub, or `JAWA_FORGEJO_TOKEN` for Forgejo;
these credentials stay on the server. Loading is bounded to 20 seconds and 20
pages of 100 API entries; larger lists report a loading error rather than show
an incomplete selector.

**Planning is optional:** drag Todo directly to Building for clear tasks, or move
it to Planning for repository investigation and refinement. Open the planning
card to read the proposed specification, answer questions, and review versioned
plans. **Approve & build** freezes the displayed revision and starts implementation;
dragging Planning to Building opens that review rather than bypassing approval.
Replies resume the same durable planning run; retry and cancel are available.
The original request and discussion remain separate from the approved specification.

Planning uses one durable BONNIE task/run/attempt: `clarify(questions)` waits
indefinitely for input, and `proposeSpec(spec, repo_commit)` waits for review
without questions. Replies require the displayed interaction token and persist
before exact-suspension Answer dispatch; no new task is created on reply.
Approval freezes the reviewed revision and atomically queues a planning approval
Answer plus a distinct implementation task. Natural planning completion after
approval cannot change the build. Typed reports, not natural text, are authoritative;
there is no parser fallback. Planning does not create PRs or use build verification. **Read-only planning is instructed, not sandbox-enforced
by the orchestrator:** BONNIE v0.22 has no per-task tool restrictions. Configure
agent credentials and tools appropriately; shared build-capable agents are not a
security boundary. See [Planning contract](internal/orchestrator/PLANNING_HANDOFF.md).

Database upgrades use embedded Goose migrations; back up existing databases before
upgrading. Generated sqlc queries and regeneration instructions are documented in
[Database tooling](internal/orchestrator/db/README.md).

Moving a Todo card to **Building** atomically creates a durable execution attempt
and outbox task. The orchestrator publishes it to `bonnie.tasks`, consumes BONNIE
events/results, and shows agent assignment, PR links, blockers, and attempt history.
Agents appear live through the BONNIE presence API, enabled by default. Set
`JAWA_NATS_PRESENCE=false` only to opt out of discovery:

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
an already-pinned result. Natural-language outcomes cannot establish completion;
only accepted typed tool reports can supply the PR used for verification.
Provider-confirmed merged PRs also count as Done, provided repository, branch, base,
and head identity checks pass. Closed unmerged PRs remain blocked.

The UI separates **Board** (one project at a time), **Archive**, **Runs**, **Agents**, and
**Settings** into bookmarkable views. It uses templ and prebuilt shadcn-templ
components, with a dark Jawa-inspired robe-brown/sand/amber palette, compact
cards, keyboard-accessible action menus, and self-hosted Inter typography.
Datastar SSE morphs only each view's keyed content, sidebar counts, and agent
chip. Rocket (Datastar v1.0.4 + Rocket beta.2) wraps SortableJS, menus, relative
timestamps, clipboard controls, and keyboard shortcuts. No production CDN is used.

On phones the board stacks its lanes; tablets use two columns, with visible card
actions and touch-sized controls. Runs retain their agent, attempt, PR and update
information in a compact card layout. Card menus allow moves without dragging.

Every card has an **Archive** button beside its actions menu. Archived cards are
hidden from Board and appear in the searchable `/archive` view. **Restore** returns
a card to its existing workflow lane. Archiving changes visibility only: it does
not cancel active planning/builds or delete plans, runs, PR links, or history.
Merge Conflict, Merged, and Checks Failed PR status evidence uses compact badges;
Checks Failed indicates a provider status-check failure, not a claim about CI results.

Jawa can be installed as a **PWA** using your browser's install command (on iOS,
Safari **Share → Add to Home Screen**). Serve it over HTTPS for mobile installation;
HTTP loopback works for local development, but HTTP on a LAN address does not
support service workers. The installed app still requires a connection to the
orchestrator. Only public static assets and a generic offline page are cached:
no task data, authenticated HTML, API responses or offline mutations. A new asset
build creates a versioned cache; close old app tabs/windows and reopen to activate
a waiting update. Installation availability depends on the browser/platform.

Keyboard shortcuts: `c` opens a new card, `/` focuses the board filter, and
`g` followed by `b`, `r`, `a`, or `s` opens Board, Runs, Agents, or Settings.
Shortcuts are disabled while typing or using a dialog. Card actions are in the
ellipsis menu; detailed execution history lives in Runs, not inside board cards.
See [`internal/orchestrator/CONTRACT.md`](internal/orchestrator/CONTRACT.md) for
UI regeneration and browser smoke-test instructions.

Keep HTTP and NATS on loopback unless protected by appropriate TLS and network
controls. There is no native orchestrator TLS configuration yet. Keep the data
directory private and back it up; do not share it with agent workspaces.

## Agent CLI configuration

The default command and `jawa serve` accept the same flags. Flags override
`.env`/exported environment defaults; exported environment values override `.env`.
Model selection already uses BONNIE's built-in `--model` flag:

```sh
jawa serve --model anthropic/claude-sonnet-latest --nats-agent-id jawa-1
```

| Environment variable | Equivalent flag |
| --- | --- |
| `JAWA_MODEL` | `--model` |
| `JAWA_HTTP_ADDR` | `--addr` |
| `JAWA_NAME` | `--name` |
| `NATS_URL` | `--nats-url` |
| `NATS_USERNAME`, `NATS_PASSWORD` | `--nats-username`, `--nats-password` |
| `NATS_TOKEN`, `NATS_NKEY_SEED` | `--nats-token`, `--nats-nkey-seed` |
| `JAWA_NATS_AGENT_ID` | `--nats-agent-id` |
| `JAWA_NATS_ROOT_SUBJECT` | `--nats-root-subject` |
| `JAWA_NATS_CONSUMER` | `--nats-consumer` |
| `JAWA_NATS_CREATE_STREAM` | `--nats-create-stream` |
| `JAWA_NATS_PRESENCE` | `--nats-presence` |
| `JAWA_NATS_PRESENCE_BUCKET` | `--nats-presence-bucket` |
| `JAWA_SANDBOX_ROOT` | `--sandbox-root` |
| `JAWA_SANDBOX_IMAGE` | `--sandbox-image` |
| `GITHUB_TOKEN` | `--github-token` |

Use `--provider-api-key` to override the selected model provider's credential
(e.g. `OPENCODE_API_KEY`, `ANTHROPIC_API_KEY`, or `OPENAI_API_KEY`) and
`--provider-url` to override its endpoint. Provider-specific authentication
settings such as cloud credential chains remain managed by the provider SDK.
Use `--nats-presence=false` or `--nats-create-stream=false` to disable booleans.
Explicit empty credential flags clear their environment defaults; when switching
NATS authentication methods, clear any conflicting environment-sourced credentials.
Prefer environment variables for secrets: CLI arguments can appear in shell
history and process listings. Secret defaults are omitted from help.

Orchestrator flags and their dashboard precedence are documented in
[`internal/orchestrator/CLI.md`](internal/orchestrator/CLI.md); task submission
flags are documented in [`cmd/submit/CLI.md`](cmd/submit/CLI.md).

## Run both services with Docker Compose

[`compose.example.yaml`](compose.example.yaml) runs the orchestrator and one
agent as separate containers using `ghcr.io/mark3labs/jawa:latest`. Each has its
own persistent volume. Only the dashboard is published, on host loopback;
the agent connects to embedded NATS over the Compose network.

1. Copy `.env.example` to `.env` if you do not already have one. Set your
   `OPENCODE_API_KEY` (or adapt the agent environment for another model provider)
   and `GITHUB_TOKEN`. Set `NATS_USERNAME=worker` and choose a `NATS_PASSWORD`
   of 12–72 bytes. The sample overrides `NATS_URL` to reach the orchestrator.
2. Start only the orchestrator:

   ```sh
   docker compose -f compose.example.yaml up -d orchestrator
   ```

3. Open http://127.0.0.1:8080, create an admin account, then complete **Settings**
   using the same NATS username/password as `.env`. The orchestrator does not
   provision these credentials from the agent's environment variables.
4. Start the agent:

   ```sh
   docker compose -f compose.example.yaml up -d agent
   docker compose -f compose.example.yaml logs -f agent
   ```

After initial setup, `docker compose -f compose.example.yaml up -d` starts both
services. `depends_on` controls startup order, not NATS readiness; if the agent
starts before NATS is ready, its restart policy retries the process. After
changing agent credentials in `.env`, run `up -d agent` again to recreate it.
Stop with `docker compose -f compose.example.yaml down`; do not add `-v` unless
you intend to delete both services' persistent data. Run trusted tasks only:
agent coding commands can access its environment and filesystem. For remote
dashboard access, use an authenticated TLS reverse proxy or SSH tunnel rather
than exposing these ports directly.

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
go install github.com/mark3labs/bonnie/cmd/bonnie@v0.23.0
bonnie chat
```

## Configuration

The Jawa binary loads `.env` from its current working directory before reading
configuration. Existing environment variables take precedence. A missing `.env`
is allowed; unreadable or invalid files stop startup without printing their contents.

| Variable | Default / purpose |
| --- | --- |
| `JAWA_NAME` | `jawa`; agent display name (`--name` overrides) |
| `JAWA_GIT_USER_NAME` | Optional agent-wide Git author and committer name |
| `JAWA_GIT_USER_EMAIL` | Optional agent-wide Git author and committer email |
| `JAWA_MODEL` | `opencode/glm-5.3-flash` |
| `JAWA_NATS_AGENT_ID` | Required, e.g. `jawa-1` (letters, digits, hyphens); `--nats-agent-id` overrides. |
| `JAWA_NATS_ROOT_SUBJECT` | `bonnie` |
| `JAWA_NATS_CREATE_STREAM` | `true` |
| `JAWA_NATS_CONSUMER` | Optional shared task consumer override |
| `JAWA_NATS_PRESENCE` | `true`; set `false` to disable BONNIE JetStream KV agent discovery |
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
**Pull requests** read/write permissions.

### Agent Git identity

Set the identity for new ordinary commits at runtime, without rebuilding the image:

```dotenv
JAWA_GIT_USER_NAME="Jawa Bot"
JAWA_GIT_USER_EMAIL=jawa-bot@example.com
```

For Docker, put these settings in `.env` and pass `--env-file .env` to
`docker run`, or export them and add `-e JAWA_GIT_USER_NAME -e JAWA_GIT_USER_EMAIL`
to the invocation above. `compose.example.yaml` forwards both settings only to
the agent service. Native deployments load them from the working directory's
`.env` or exported environment (exported values take precedence).

Each nonempty setting supplies both the corresponding `GIT_AUTHOR_*` and
`GIT_COMMITTER_*` variables to coding commands. Name and email are independent:
an unset or empty component injects nothing and retains Git's normal fallback.
Explicit Jawa settings override inherited standard Git identity variables and
repository/global/system identity for ordinary commits. Explicit Git command
options and operations that preserve an existing author retain their usual
semantics. Supplied values are not trimmed or shell-interpolated.

The standard Docker image retains `Jawa <jawa@bonnie>` as its system Git fallback;
native deployments retain their existing Git environment and configuration.
No Git configuration files or process environment are rewritten. These are
agent-wide, environment-only settings, not CLI flags; restart the native agent
or recreate the container after changing them (for Compose, run
`docker compose -f compose.example.yaml up -d --force-recreate agent`).

Commit identity is metadata, not authentication or signing, and does not change
task routing or commit/push authorization. Use your desired verified or noreply
email for Git host attribution; GitHub access still uses `GITHUB_TOKEN`.

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

### Dashboard orchestrator overrides

The authenticated Settings page saves dashboard overrides for the running
orchestrator's HTTP/NATS listener addresses, debug logging, GitHub/Forgejo
read-only provider tokens, and GitHub's `JAWA_ALLOW_NO_CI` opt-in. Dashboard
values override explicit `orchestrator` CLI flags, which override the
`JAWA_ORCHESTRATOR_LISTEN`, `JAWA_ORCHESTRATOR_NATS_LISTEN`, and
`JAWA_ORCHESTRATOR_DEBUG` environment defaults. `JAWA_ORCHESTRATOR_DATA_DIR`
selects the bootstrap database directory; `--data-dir` overrides it. The data
directory is needed to load these saved settings, so its selection is not a
dashboard override. Listener changes take effect after restart; debug logging
and provider settings resolve immediately, without mutating process environment.
Provider tokens are write-only in the UI; explicit clear-token controls remove
the dashboard override and reveal the environment fallback. Reset all removes
every dashboard override. These settings apply only to the `orchestrator`
command, not standalone agent/`serve` or administrative subcommands.

### Continue after an execution failure

Planning, Building and Fix card menus can offer **Resume failed run** for eligible
execution failures; the planning detail action remains available. Resume preserves
the same run, saved context and branch, including the original Fix repair branch,
PR assignment and context. **Retry attempt / Retry planning** still starts fresh work. Blockers use
guidance instead (below); completed/cancelled runs and ambiguous ownership cannot
be resumed. See [failed-run recovery](internal/orchestrator/FAILED_RESUME.md) for
agent compatibility, pending-command safety and operational limitations.

### Guide a blocked agent

An agent that calls `reportBlocker` pauses its run instead of ending it. The
card shows **Blocked:** with the agent's reason and offers **Reply with
guidance…** (card note and menu). Your guidance is delivered into the *same*
run, keeping its conversation, branch and work so far; the agent may then
finish, ask again, or block again. Planning blockers appear on the planning
page with a **Reply with guidance** form. To abandon a paused build instead,
choose **Cancel run…**, then **Retry attempt** or **Reset to Todo**. See
[blocker guidance](internal/orchestrator/BLOCKER_GUIDANCE.md) for details.

### Repair an existing pull request

The board's **Fix** lane contains repairs to existing pull requests. On a card
in **Ready for review**, choose **Send to Fix…**, enter a required fix message,
and confirm **Send to Fix**. Dragging a verified card from Ready for review to
Fix opens the same dialog with an empty message; cancelling or pressing Escape
leaves the card and its position unchanged. A Done card without a verified pull
request cannot be sent to Fix.

A provider check reporting a merge conflict adds a warning and **Fix Merge Conflict** action with a prefilled message you can edit. Unknown or failed
checks are not displayed as conflicts. Checks never automatically move a card
or start a repair. The agent repairs the original branch and existing PR; it
must not create a replacement PR or merge it. Failed or blocked repairs can be
retried from the Fix card's action menu, preserving the repair assignment.

The authenticated, CSRF-protected `POST /cards/fix` endpoint accepts `card_id`
and a nonblank `message`. It returns the normal board redirect for native form
requests or finite Datastar SSE patches for enhanced requests. On failure the
repair dialog stays open with its draft; on success it closes and clears.

### Refresh pull request status

The **Refresh ready PRs** button at the top of **Ready for review** checks
all eligible cards in that project, rather than adding a refresh button to each card.
While the request is pending, the button shows **Refreshing ready PRs…** and a
loading indicator and prevents another submission. It becomes available again
when the request finishes, even if cards remain queued for background checking.
This action bypasses the normal one-minute polling cooldown; it does not rerun
CI, move cards, merge pull requests, or start repairs.

Current merge evidence appears separately from the historical ready badge:
checking (queued or in progress), no merge conflicts detected, confirmed
conflict, unknown, check failed, merged, or closed without merging. Cards with
no applicable evidence say they have not been checked yet. Completed checks
show a muted relative timestamp (with the exact UTC time in its tooltip) and
optional head/base revision evidence.
Checking cards await fresh evidence; unknown and failed checks never imply clear.
“No merge conflicts detected” is not a guarantee that every merge gate passes.

The authenticated, CSRF-protected `POST /prs/refresh` endpoint accepts
`project_id` and optional `card_id` (empty means the whole project). It checks
at most 16 cards within 30 seconds and durably queues the remainder for background
polling. Enhanced requests return finite Datastar SSE board patches with
**targeted / checked / queued** counts; native forms redirect back to their view.
The server gives this route a bounded 35-second request/write window. Concurrent
checks return busy; an unavailable checker returns unavailable. Provider errors
are shown as failed checks, not successful clear evidence. Interrupted batches
leave unfinished cards checking for background polling, including after restart.
A failed enhanced request refreshes current evidence when possible and retains
an actionable error notice; request cancellation may require the next live
snapshot or page reload to display the queued state.

Regenerate the dashboard after editing templates or frontend sources:

```sh
go run github.com/a-h/templ/cmd/templ@v0.3.1070 generate
cd internal/orchestrator/frontend
npm ci
npm run build
npm run test:fix  # requires Chromium; CHROMIUM_PATH overrides its location
npm run test:pr-refresh
```

### Automatic failed-run recovery

Settings → Server → **Auto-resume failed runs** is enabled by default. Planning,
build and Fix runtime failures resume the same task/run with saved conversation,
context and branch, after delays of 5, 10, 20, 40 and 80 seconds (maximum five
automatic commands per task). Typed blockers, cancellation, waiting, interruption,
missing reports and ambiguous ownership are not auto-resumed. Manual resume does
not consume the budget; fresh Retry starts a new task/budget. Fix retains its
existing repair PR, branch and message.

The setting applies immediately. Disabling prevents new automatic reservations,
not delivery of commands already reserved; schedules and budgets survive restart
and disabling. Enabling also schedules existing eligible failures, and may process
overdue schedules. Reset dashboard overrides restores enabled behavior. Host
rejection stops that failure generation; exhausted budgets need operator action.
Default-on recovery can incur provider costs and repeat external side effects.
It still requires the original agent journal/sandbox and compatible control route;
see [failed-run recovery limits](internal/orchestrator/FAILED_RESUME.md).

### Manually marking a Ready PR as merged

The corner menu of a Ready for review card with a verified PR and no active
attempt includes **Mark as merged**. This is a local display override: it does
not merge or edit the remote PR, publish an agent command, change readiness or
attempt history, move the card, or archive it. It works even when the provider
is unavailable or still reports the PR as open. The card shows **Marked as
merged manually** with the marking time, not a provider-check timestamp.

The marker persists across restarts and archive/restore and takes precedence
over provider status. Refresh still fetches provider evidence without clearing
the manual marker. It applies only to the exact latest ready attempt, PR URL,
and current provider/repository/base configuration; a repair or configuration
change does not inherit it. Duplicate submissions preserve the original time.
There is no manual undo action, and Send to Fix remains available as before.

`POST /cards/mark-merged` uses dashboard authentication and CSRF protection.
The native/enhanced form sends `card_id`, `expected_attempt_id`,
`expected_pr_url`, `_csrf`, and the standard view/project fields. Stale or
ineligible submissions fail without writing metadata. Already effectively
merged cards omit the action.
