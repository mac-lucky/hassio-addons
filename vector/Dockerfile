ARG BUILD_FROM
# hadolint ignore=DL3006
FROM ${BUILD_FROM}

# Set shell with pipefail
SHELL ["/bin/ash", "-o", "pipefail", "-c"]

# Build arguments
ARG BUILD_ARCH
ARG VECTOR_VERSION="0.44.0"

# Environment
ENV VECTOR_CONFIG=/etc/vector/vector.yaml

# hadolint ignore=DL3018
RUN set -eux; \
    \
    # Determine architecture for Vector download
    case "${BUILD_ARCH}" in \
        amd64) VECTOR_ARCH="x86_64-unknown-linux-musl" ;; \
        aarch64) VECTOR_ARCH="aarch64-unknown-linux-musl" ;; \
        *) echo "Unsupported architecture: ${BUILD_ARCH}" && exit 1 ;; \
    esac; \
    \
    # Download and install Vector
    curl -sSL "https://packages.timber.io/vector/${VECTOR_VERSION}/vector-${VECTOR_VERSION}-${VECTOR_ARCH}.tar.gz" \
        | tar xzf - --strip-components=2 -C /usr/local/bin/ \
            "./vector-${VECTOR_ARCH}/bin/vector"; \
    \
    # Verify installation
    /usr/local/bin/vector --version; \
    \
    # Create required directories
    mkdir -p /etc/vector /var/lib/vector; \
    \
    # Install additional dependencies
    apk add --no-cache \
        jq=1.7.1-r0

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
