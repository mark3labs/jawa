# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.0
ARG RUST_VERSION=1

# Forgejo CLI (fj). Upstream Linux binaries require glibc 2.39, newer than
# bookworm's, so build it against the runtime's Debian release instead.
FROM rust:${RUST_VERSION}-bookworm AS fj
ARG FJ_VERSION=0.6.0
RUN cargo install forgejo-cli --version "${FJ_VERSION}" --locked --root /out \
    && strip /out/bin/fj

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
RUN go install github.com/mark3labs/bonnie/cmd/bonnie@v0.27.0
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
# Gitea/Forgejo CLI (tea): static Go binaries with pinned checksums.
ARG TEA_VERSION=0.16.0
ARG TEA_SHA256_AMD64=9c142c8a0b31485eea2387031c1c404f12b7141075eb4ffbd7ced49441590636
ARG TEA_SHA256_ARM64=0e14a036322eeb1fc55e266acafeb8490e523ed99f9232034ae74861e2339879
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates curl git gnupg build-essential pkg-config ripgrep \
       bash file time python3 python3-pip python3-venv python3-pil \
       libssl3 zlib1g \
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
RUN set -eu; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in \
      amd64) sha="$TEA_SHA256_AMD64" ;; \
      arm64) sha="$TEA_SHA256_ARM64" ;; \
      *) echo "Unsupported tea architecture: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fSL --retry 3 "https://gitea.com/gitea/tea/releases/download/v${TEA_VERSION}/tea-${TEA_VERSION}-linux-${arch}" \
      -o /usr/local/bin/tea; \
    printf '%s  %s\n' "$sha" /usr/local/bin/tea | sha256sum -c -; \
    chmod 0755 /usr/local/bin/tea; \
    tea --version
# Debian login shells reset PATH; BONNIE uses sh -lc for coding commands.
# Put Go on the standard path so it works regardless of profile behavior.
RUN ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt
# Default Git identity for coding commands.
RUN git config --system user.name "Jawa" \
    && git config --system user.email "jawa@bonnie"
RUN useradd --create-home --uid 10001 --shell /bin/bash jawa \
    && mkdir -p /data/.bonnie/workspaces /workspace \
    && chown -R jawa:jawa /data /workspace \
    && ln -s /data/.bonnie/workspaces /w
COPY --from=build /out/jawa /out/gopls /out/golangci-lint /usr/local/bin/
COPY --from=fj /out/bin/fj /usr/local/bin/
ENV LIGHTPANDA_DISABLE_TELEMETRY=true \
    GH_PROMPT_DISABLED=1 \
    JAWA_HTTP_ADDR=0.0.0.0:8080 \
    JAWA_SANDBOX_ROOT=/w \
    TMPDIR=/tmp \
    PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
EXPOSE 8080
# Pass NATS_URL, NATS_USERNAME, NATS_PASSWORD, GITHUB_TOKEN, FORGEJO_TOKEN and model-provider
# credentials at runtime. Never use build arguments for credentials.
USER jawa
WORKDIR /data
# Check the same login-shell mode the agent uses, as the runtime user.
RUN sh -lc 'command -v go && command -v gofmt && go version && gh --version && tea --version && fj version && gopls version && golangci-lint version'
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/jawa"]
