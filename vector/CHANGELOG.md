# Changelog

All notable changes to this add-on will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.6.1] - 2026-07-31

### Changed

- Vector bumped to 0.57.0
- The journald source now sets `current_boot_only: true`. Vector 0.57 refuses to
  start with `current_boot_only: false` on systemd versions 250 through 257 (the
  base image ships 252), so only the current boot's logs are collected instead
  of the full journal history across reboots

## [1.6.0] - 2026-07-30

### Security

- The VictoriaLogs password is no longer written into the generated Vector
  configuration. It is passed to Vector through the environment instead, so a
  failed validation can no longer print it to the add-on log
- The generated configuration is now printed with credentials redacted when
  validation fails, and any `user:pass@` in the endpoint URL is masked, including
  in the errors Vector itself prints
- The credentials are no longer placed in the environment at all when
  `custom_config_path` is set. A custom config is arbitrary YAML from `/share`,
  which any share-mapped add-on can write, and Vector interpolates environment
  variables into it before parsing
- A failed custom configuration is not printed to the log. It is user-supplied
  YAML that can hold keys and block scalars the redactor does not know about
- `vector.yaml` is created with mode 600 before anything is written to it

### Fixed

- Username and password are escaped for YAML instead of being interpolated raw.
  A quote, backslash or dollar sign in either no longer breaks the configuration
  or silently changes it
- A password containing an `@` no longer has its tail printed to the log when the
  endpoint carries credentials. The mask stopped at the first `@` instead of the
  one that actually delimits the userinfo
- A password with a trailing newline is rejected instead of being silently
  truncated. The check now runs against the raw JSON, because the shell strips
  the newline before the value reaches a variable. Carriage return and NEL are
  rejected too; both are YAML line breaks
- Log messages that are missing or not a string no longer abort the transform.
  Those events used to pass through with nothing applied to them, so they
  reached VictoriaLogs unenriched and, more importantly, unredacted
- Journal entries with no message no longer have the whole event encoded into
  the message field, which pushed metadata such as `_CMDLINE` past a redaction
  pass written for message text
- `collect_journal: false` and `redact_sensitive: false` were ignored. jq's
  alternative operator treats `false` as empty, so an option explicitly set to
  false read back as true and redaction could not be turned off
- An empty `stream_fields` list produced an invalid `_stream_fields:` key and
  failed validation
- Systemd unit names are quoted in the generated config, so a unit starting with
  `@` no longer breaks the YAML
- A username set without a password, or a password without a username, now logs
  a warning instead of silently producing an auth header the server rejects

### Changed

- `custom_config_path` pointing at a file that does not exist is now a fatal
  error. Previously the add-on silently fell back to the generated configuration
  with nothing in the log to say so
- Documentation now matches the options that actually exist: the removed
  `collect_docker` options are gone, `redact_sensitive` is documented, and the
  endpoint examples include the required `/insert/elasticsearch` path
- The enrichment VRL is written to its own file, `/etc/vector/enrich.vrl`, which
  the transform references instead of embedding the program. The generated
  `vector.yaml` is now about 40 lines instead of 100, and the VRL can be run on
  its own with `vector vrl --program`

### Added

- A generator test suite under `vector/tests`, run in CI against the built image.
  It covers the things `vector validate` cannot see: that the password reaches
  neither the config file nor the log, and how the generated VRL behaves on
  events with a missing or non-string message

## [1.5.1] - 2026-01-12

### Fixed

- Log level extraction now parses message content instead of relying solely on syslog PRIORITY
- Home Assistant logs with "WARNING" in message now correctly set `level: "warn"` instead of `level: "error"`

### Changed

- Level extraction priority: message content patterns take precedence over syslog PRIORITY field
- Normalized level values: `warning` to `warn`, `critical`/`fatal` to `error`

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
