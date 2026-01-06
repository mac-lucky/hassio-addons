# Changelog

All notable changes to this add-on will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.4.0] - 2026-01-06

### Added

- Basic authentication support for VictoriaLogs endpoint
- New configuration options: `victorialogs_username` and `victorialogs_password`

## [1.0.0] - 2025-01-05

### Added

- Initial release
- Systemd journal log collection
- Docker container log collection
- VictoriaLogs sink with Elasticsearch-compatible API
- Configurable filtering by unit/container name
- Custom labels support
- Configuration validation before startup
- Support for custom Vector configuration files
- Multi-architecture support (amd64, aarch64, armv7)

### Technical Details

- Vector version: 0.44.0
- Base image: ghcr.io/hassio-addons/base:16.3.2
