# Both ARGs must stay declared here, before the first FROM: buildkit only
# resolves an ARG used in a FROM instruction's image name from this global
# scope, not from an ARG declared between stages further down the file.
ARG GO_VERSION=1.27.1
ARG BUILD_FROM=alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# Keep in sync with config.yaml's version (which has a comment pointing back
# here) - stamped into the binary as main.version below.
ARG BUILD_VERSION=0.6.9

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
# The base image pins every package it installed in /etc/apk/world
# (libssl3=3.3.3-r0, curl=8.12.1-r1, ...), which is why a plain `apk upgrade`
# left the OpenSSL and curl fixes on the shelf through two releases. Drop the
# pins first; the base image's versions are a build snapshot, not a
# compatibility requirement.
RUN sed -i 's/=.*$//' /etc/apk/world && apk upgrade --no-cache && apk add --no-cache git ca-certificates sops

COPY --from=builder /ha-gitops-agent /ha-gitops-agent

ENTRYPOINT ["/ha-gitops-agent"]
