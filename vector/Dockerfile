# Stage 1: Get Vector binary from official Alpine image
ARG VECTOR_VERSION="0.44.0"
FROM timberio/vector:${VECTOR_VERSION}-alpine AS vector

# Stage 2: Final image
ARG BUILD_FROM
# hadolint ignore=DL3006
FROM ${BUILD_FROM}

# Set shell with pipefail
SHELL ["/bin/ash", "-o", "pipefail", "-c"]

# Build arguments
ARG BUILD_ARCH

# Environment
ENV VECTOR_CONFIG=/etc/vector/vector.yaml

# Copy Vector binary from official Alpine image (already musl-compatible)
COPY --from=vector /usr/local/bin/vector /usr/local/bin/vector

# hadolint ignore=DL3018
RUN set -eux; \
    \
    # Install dependencies
    apk add --no-cache \
        jq=1.7.1-r0; \
    \
    # Create required directories
    mkdir -p /etc/vector /var/lib/vector; \
    \
    # Verify installation
    /usr/local/bin/vector --version

# Copy rootfs
COPY rootfs /

# Set permissions
RUN chmod +x /etc/s6-overlay/s6-rc.d/vector/run \
    && chmod +x /etc/s6-overlay/s6-rc.d/vector/finish \
    && chmod +x /usr/bin/generate-config.sh

# Build arguments for labels
ARG BUILD_DATE
ARG BUILD_DESCRIPTION
ARG BUILD_NAME
ARG BUILD_REF
ARG BUILD_REPOSITORY
ARG BUILD_VERSION

# Labels
LABEL \
    io.hass.name="${BUILD_NAME}" \
    io.hass.description="${BUILD_DESCRIPTION}" \
    io.hass.arch="${BUILD_ARCH}" \
    io.hass.type="addon" \
    io.hass.version=${BUILD_VERSION} \
    maintainer="Mac Lucky" \
    org.opencontainers.image.title="${BUILD_NAME}" \
    org.opencontainers.image.description="${BUILD_DESCRIPTION}" \
    org.opencontainers.image.vendor="Mac Lucky" \
    org.opencontainers.image.authors="Mac Lucky" \
    org.opencontainers.image.licenses="MIT" \
    org.opencontainers.image.source="https://github.com/${BUILD_REPOSITORY}" \
    org.opencontainers.image.documentation="https://github.com/${BUILD_REPOSITORY}/blob/main/vector/DOCS.md" \
    org.opencontainers.image.created=${BUILD_DATE} \
    org.opencontainers.image.revision=${BUILD_REF} \
    org.opencontainers.image.version=${BUILD_VERSION}
