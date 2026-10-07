# Jawa

![Jawa logo](logo.png)

A coding agent built on [BONNIE](https://github.com/mark3labs/bonnie). Jawa receives
work over NATS, edits code, runs tests, and can push branches and create pull
requests. Lightpanda MCP provides browser tools.

The image includes Go, Git, GitHub CLI, Go linters, Python, and common development
utilities.

## Run with Docker

Configure your environment:

```sh
export NATS_URL=tls://your-nats-server:4222
export NATS_USERNAME=worker
export NATS_PASSWORD='your-password'
export OPENCODE_API_KEY='your-api-key'
export GITHUB_TOKEN='your-github-token'
export JAWA_NATS_WORKER_ID=jawa-1
```

Start Jawa:

```sh
docker run -d --init --name jawa \
  -p 127.0.0.1:8080:8080 \
  -v jawa-data:/data \
  -e NATS_URL -e NATS_USERNAME -e NATS_PASSWORD \
  -e OPENCODE_API_KEY -e GITHUB_TOKEN -e JAWA_NATS_WORKER_ID \
  ghcr.io/mark3labs/jawa:latest
```

Use a JetStream-enabled NATS server. The worker creates the required streams,
so its account needs stream/consumer administration and publish/subscribe
permissions for the `bonnie` protocol routes, worker routes, reply inboxes, and
JetStream API/acknowledgements.

Each instance needs a unique, stable worker ID and its own data volume. Jawa uses
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
go run ./cmd/submit -worker jawa-1 \
  -text 'Clone OWNER/REPO, implement the requested change, test it, and open a PR. You are authorized to push a branch and create the PR.'
```

The client logs pickup and status changes, then prints the result. Use `-id` to
identify a request and `-timeout` to adjust the wait (default: 30 minutes).
Targeted tasks wait for their assigned worker. Delivery is at least once; make
external effects safe to repeat.

For an existing input stream, ensure its subjects include `bonnie.tasks.worker.*`
before enabling targeted workers. BONNIE validates existing streams rather than
modifying them.

## Chat locally

Install the BONNIE CLI, then connect to the published HTTP port:

```sh
go install github.com/mark3labs/bonnie/cmd/bonnie@v0.16.0
bonnie chat
```

## Configuration

| Variable | Default / purpose |
| --- | --- |
| `JAWA_MODEL` | `opencode/glm-5.3-flash` |
| `JAWA_NATS_WORKER_ID` | Required, e.g. `jawa-1` (letters, digits, hyphens) |
| `JAWA_NATS_ROOT_SUBJECT` | `bonnie` |
| `JAWA_NATS_CREATE_STREAM` | `true` |
| `JAWA_NATS_CONSUMER` | Optional shared task consumer override |

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

Install the [microsandbox](https://github.com/superradcompany/microsandbox)
`msb` CLI on the agent host and configure its Linux/KVM requirements first.
Each guest uses `ghcr.io/mark3labs/jawa:latest`, 4 GiB RAM, and 2 CPUs, providing
the same coding tools as the published image. Ensure the host can pull it from
GHCR. Local remains available with `--sandbox local`.

The stock Jawa container does not include `msb` or configure KVM. To select
microsandbox inside a container, its runtime must provide those prerequisites;
passing the flag alone is not sufficient. Lightpanda MCP runs alongside the
agent, while coding commands and completion checks run inside the selected
backend. Preserve backend state and avoid changing backends for in-flight runs.

## Develop

Requires Go 1.27+. Edit `instructions.md` for agent behavior and `main.go` for
runtime configuration.

```sh
go test ./...
go vet ./...
docker build -t jawa:local .
```

Pushes publish images to GHCR with branch and commit tags; `master` updates
`latest`.
