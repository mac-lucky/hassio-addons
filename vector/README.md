# Home Assistant Add-on: Vector Log Collector

[![Release](https://img.shields.io/github/v/release/mac-lucky/hassio-addons?filter=vector*)](https://github.com/mac-lucky/hassio-addons/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

![Vector Logo](https://vector.dev/img/logo-light.svg)

High-performance log collector that sends Home Assistant logs to VictoriaLogs.

## About

[Vector](https://vector.dev/) is a high-performance, end-to-end observability data pipeline written in Rust. It's designed to collect, transform, and route logs and metrics with minimal resource usage.

This add-on configures Vector to collect logs from your Home Assistant system and send them to [VictoriaLogs](https://docs.victoriametrics.com/victorialogs/), a fast and cost-effective log management solution.

## Features

- Collects systemd journal logs (HA Core, Supervisor, add-ons)
- Collects Docker container logs
- Low memory footprint (~30-50MB)
- Automatic log enrichment with host/container metadata
- Configurable filtering and labeling
- Built-in configuration validation

## Quick Start

1. Install the add-on
2. Set `victorialogs_endpoint` to your VictoriaLogs URL
3. Start the add-on
4. Query logs in VictoriaLogs

## Documentation

See [DOCS.md](DOCS.md) for full documentation.

## Support

- [Issue Tracker](https://github.com/mac-lucky/hassio-addons/issues)
- [Vector Documentation](https://vector.dev/docs/)
- [VictoriaLogs Documentation](https://docs.victoriametrics.com/victorialogs/)
