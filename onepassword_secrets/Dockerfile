# These ARGs must stay declared here, before the first FROM: buildkit only
# resolves an ARG used in a FROM image name from this global scope. No
# defaults: CI passes them from build.yaml (args and build_from), the only
# pin, so a local docker build passes them the same way.
ARG GO_VERSION
ARG BUILD_FROM
ARG CONNECT_VERSION

# Keep in sync with config.yaml's version - stamped into the binary below.
ARG BUILD_VERSION=0.1.7

FROM golang:${GO_VERSION}-alpine AS builder
ARG BUILD_VERSION

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${BUILD_VERSION}" -o /onepassword-secrets ./cmd/onepassword-secrets

# 1Password's own Connect images, only to copy the two binaries out.
FROM 1password/connect-api:${CONNECT_VERSION} AS connect-api
FROM 1password/connect-sync:${CONNECT_VERSION} AS connect-sync

# hadolint ignore=DL3006
FROM ${BUILD_FROM}
ARG BUILD_VERSION

LABEL org.opencontainers.image.title="1Password Secrets" \
      org.opencontainers.image.description="Keeps Home Assistant's secrets files in sync with 1Password through a built-in Connect server." \
      org.opencontainers.image.source="https://github.com/mac-lucky/hassio-addons"

# PKG_REFRESH changes on every CI run, so this layer (and the upgrade in
# it) never comes from a cache: the pinned debian-base lags Debian's
# security fixes, and a cached upgrade layer is only as fresh as the build
# that made it.
ARG PKG_REFRESH
# hadolint ignore=DL3005,DL3008
RUN apt-get update \
    && apt-get upgrade -y --no-install-recommends \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=connect-api /usr/bin/connect-api /usr/local/bin/connect-api
COPY --from=connect-sync /usr/bin/connect-sync /usr/local/bin/connect-sync
COPY --from=builder /onepassword-secrets /onepassword-secrets

ENTRYPOINT ["/onepassword-secrets"]
