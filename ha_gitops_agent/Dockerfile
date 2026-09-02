# Both ARGs must stay declared here, before the first FROM: buildkit only
# resolves an ARG used in a FROM instruction's image name from this global
# scope, not from an ARG declared between stages further down the file.
ARG GO_VERSION=1.27.0
ARG BUILD_FROM=alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# Keep in sync with config.yaml's version (which has a comment pointing back
# here) - stamped into the binary as main.version below.
ARG BUILD_VERSION=0.6.5

FROM golang:${GO_VERSION}-alpine AS builder
ARG BUILD_VERSION

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${BUILD_VERSION}" -o /ha-gitops-agent ./cmd/ha-gitops-agent

FROM ${BUILD_FROM}
ARG BUILD_VERSION

LABEL org.opencontainers.image.title="GitOps Agent" \
      org.opencontainers.image.description="Sync Home Assistant YAML config from a git repository, with dry-run diff, validate-then-apply, and rollback." \
      org.opencontainers.image.source="https://github.com/mac-lucky/hassio-addons"

# sops is what encrypts and decrypts secret values (see internal/sopscrypt).
# Only used when age_key is configured, but it has to be in the image either
# way: the agent probes for it at startup and refuses to run with a key it
# cannot act on.
# apk upgrade first: the digest-pinned base can lag Alpine package fixes, and
# upgrading at build picks them up without waiting for a base-image rebuild.
RUN apk upgrade --no-cache && apk add --no-cache git ca-certificates sops

COPY --from=builder /ha-gitops-agent /ha-gitops-agent

ENTRYPOINT ["/ha-gitops-agent"]
