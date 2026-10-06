# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.0
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
RUN go install github.com/mark3labs/bonnie/cmd/bonnie@v0.13.0
COPY . .
RUN bonnie build --output /out/jawa
RUN GOBIN=/out go install golang.org/x/tools/gopls@v0.23.0 \
    && GOBIN=/out go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

# Keep the Go toolchain available for coding tasks, not just the agent build.
FROM golang:${GO_VERSION}-bookworm AS runtime
ARG TARGETARCH
# Upstream currently distributes Linux binaries via its nightly release.
ARG LIGHTPANDA_VERSION=nightly
ARG LIGHTPANDA_SHA256
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates curl git gnupg build-essential pkg-config ripgrep \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
       -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod a+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
       > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in \
      amd64) asset=lightpanda-x86_64-linux ;; \
      arm64) asset=lightpanda-aarch64-linux ;; \
      *) echo "Unsupported Lightpanda architecture: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fSL --retry 3 "https://github.com/lightpanda-io/browser/releases/download/${LIGHTPANDA_VERSION}/${asset}" \
      -o /usr/local/bin/lightpanda; \
    if [ -n "${LIGHTPANDA_SHA256:-}" ]; then \
      printf '%s  %s\n' "$LIGHTPANDA_SHA256" /usr/local/bin/lightpanda | sha256sum -c -; \
    fi; \
    chmod 0755 /usr/local/bin/lightpanda; \
    lightpanda --help >/dev/null
# System config is readable inside BONNIE's per-run sandbox (HOME is per run).
RUN git config --system user.name "Jawa" \
    && git config --system user.email "jawa@bonnie"
RUN useradd --create-home --uid 10001 --shell /bin/bash jawa \
    && mkdir -p /data/.bonnie \
    && chown -R jawa:jawa /data
COPY --from=build /out/jawa /out/gopls /out/golangci-lint /usr/local/bin/
ENV LIGHTPANDA_DISABLE_TELEMETRY=true \
    GH_PROMPT_DISABLED=1 \
    JAWA_HTTP_ADDR=0.0.0.0:8080
EXPOSE 8080
# Pass NATS_URL, NATS_USERNAME, NATS_PASSWORD, GITHUB_TOKEN and model-provider
# credentials at runtime. Never use build arguments for credentials.
USER jawa
WORKDIR /data
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/jawa"]
