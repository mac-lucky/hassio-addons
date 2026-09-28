# Changelog

All notable changes to this add-on will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.10.1] - 2026-09-28

### Fixed

- The SSH add-on's `Accepted key`, `Postponed publickey` and `User child is on
  pid` lines were stored as `error`. They join the rest of sshd's connection
  traffic as `info`.
- AirConnect (the AirCast and AirSonos add-ons) prints no level and writes to
  stderr, so its routine device messages were all `error`. Lines in its
  `[hh:mm:ss.mmm] Function:line` format are now `info`, unless they say
  something like `cannot`, `unable`, `failed` or `error`: its real errors look
  the same as the rest.
- Trailing whitespace is removed from messages, including the carriage return
  at the end of every sshd line.

## [1.10.0] - 2026-09-28

### Added

- `include_containers` and `exclude_containers` pick add-ons by container name.
  On Home Assistant OS every container logs through `docker.service`, so the
  unit options could never single one out. An entry matches the full name or
  its `_<slug>` ending, so `music_assistant` finds
  `app_d5369777_music_assistant` and keeps finding it if the Supervisor
  renames the prefix again.
- `multiline_containers` joins tracebacks and other multi-line output for any
  container, where 1.9.0 only did it for Core. The Supervisor is joined by
  default as well.
- Lines longer than 16 KB, which Docker splits into pieces, arrive as one entry
  again, up to 96 KB. zigbee2mqtt produced about 500 of these a week, each
  split into five or six events.

### Changed

- The level comes from the line itself in many more formats: ISO timestamps
  (Vector's own log, Go and Rust programs), cloudflared's `INF`/`WRN`/`ERR`,
  zigbee2mqtt, bashio, `s6-rc`, a bare leading level word (esphome) and logfmt
  `level=`. Vector's own log, the s6
  service lines and cloudflared's routine output were all stored as `error`,
  and zigbee2mqtt errors as `info`.
- Critical and fatal, and syslog priorities 0 to 2, now count as `error`
  rather than `emergency`, `alert` or `critical`, the same as a Home Assistant
  `CRITICAL` line already did.
- Colour codes are stripped from messages. They were in almost every Core and
  Supervisor line.
- Blank lines are dropped instead of being stored as VictoriaLogs'
  "missing _msg field" placeholder.
- Kernel and audit entries get a `unit` and `container_name` (`kernel`,
  `audit`), so they land in a stream of their own, and audit entries get a
  level (`warn` for an AppArmor denial).
- With `hostname` empty the `host` label is the host name journald reports.
  It was this add-on's own container name before, so the value changes, and
  with it the stream, for anyone who had not set `hostname`.
- Delivery is acknowledged: the journal position only moves past an entry once
  VictoriaLogs has accepted it, so a restart during an outage resends what was
  in flight instead of losing it. Such a restart can store an entry twice.
- A configuration error stops the add-on with the reason in its log, instead
  of restarting it every five seconds while it showed as running. Validation
  no longer runs sink healthchecks, so a custom config whose sink is down at
  that moment is not mistaken for a broken one.
- `log_level: error` really shows only errors; it showed warnings too.
- A message over 200 KB is cut to 50,000 characters. VictoriaLogs drops
  entries over 256 KB without saying so.

### Fixed

- Redaction no longer eats ordinary words: "secret encryption enabled" was
  logged as "secret: [REDACTED] enabled". A key now needs a `:` or `=` after
  it (`:=` and `=>` count too). Keys and values may be quoted, including JSON
  logged inside JSON and headers dumped as a Python dict. Credentials in URLs
  (including `:password@` and a bare token), `--password x` style flags and
  bare JWTs (Home Assistant access tokens) are redacted now, and the process
  command line and journald's raw copy of a rewritten message, which can both
  carry credentials, are dropped.
- An entry whose systemd unit was not a plain string skipped enrichment and
  redaction entirely.
- An extra label named like a field the add-on sets (`message`, `level`,
  `host` and so on) is refused. It used to overwrite that field on every
  entry.
- The DOCS query examples used `{instance="homeassistant"}`, which matches
  nothing because `instance` is not a stream field.
- s6-overlay no longer warns about the service bundle location on every
  start.

## [1.9.1] - 2026-09-25

### Changed

- Base image updated to hassio-addons/debian-base 9.4.0 (from 7.5.1).

## [1.9.0] - 2026-09-20

### Fixed

- Home Assistant tracebacks arrive as one event instead of one per frame. The
  journald driver stamps every stderr line PRIORITY=3, so each frame of a Python
  traceback was counted and stored as its own error: a single failure showed up
  fifteen times and the stack was spread over fifteen rows. Lines that do not
  open a new Home Assistant log line now merge into the one that does. Only the
  homeassistant container is routed through the reduce; every other unit goes
  straight to the sink. A merged entry is capped at 200 lines or ten seconds,
  and because it is only complete once the next log line arrives, an isolated
  error can arrive up to about three seconds later than before.

## [1.8.3] - 2026-09-06

### Security

- Debian packages are upgraded at image build. The pinned debian-base lagged
  Debian's fixes for openssl, gnutls and glibc, so the published image kept
  carrying vulnerabilities the distribution had already patched.

## [1.8.2] - 2026-09-02

### Fixed

- sshd connection lines from the SSH add-on no longer arrive as `error`. The
  Docker journald driver marks every stderr line PRIORITY=3 and sshd logs its
  routine connection traffic there; the level fallback now maps that chatter
  to `info` and leaves real failures alone.

## [1.8.1] - 2026-09-02

### Security

- A failing custom config no longer has its contents echoed into the add-on
  log. The validator quotes offending values back verbatim (a wrongly typed
  or misspelled field prints the literal value, credentials included), which
  contradicted the promise that a custom config is never printed. The
  validator's output is now dropped entirely; run `vector validate` against
  your file to see the details, as the log already suggested.

### Fixed

- A custom config that sets no `data_dir` failed validation on every start,
  because Vector's default directory does not exist in the image. It is
  created now; set `data_dir: /data/vector` in your config if you want the
  journald cursor to survive restarts.
- A hostname containing the literal text `__INSTANCE__` would silently take
  the instance value in shipped logs, because the two template placeholders
  were substituted one after the other. Such values are now rejected.
- An endpoint URL containing a backslash could break the generated YAML in
  confusing ways; it is now rejected up front like the other unsafe
  characters.
- A signal-killed Vector (out-of-memory, a crash) restarted instantly and
  silently, forever. The supervisor script now logs which signal ended it
  and waits five seconds, like it always did for ordinary error exits.
- `journal_include_units`/`journal_exclude_units` containing only an empty
  string were silently ignored; an empty unit name is now always an error,
  matching what already happened when it appeared next to a valid one.
- Log level detection now understands multi-parameter ANSI colour prefixes
  (bold plus colour), not just single-parameter ones.
- An empty `instance` value now says it must not be empty instead of
  claiming it contains invalid characters.

### Changed

- With `custom_config_path` set, the other options are no longer validated,
  and `victorialogs_endpoint` may be left empty - a custom config never
  needed a throwaway endpoint, now it does not have to carry one.
- The declared 8686 port mapping is gone. Vector's API binds to localhost
  inside the container, so mapping the port never exposed anything; the
  option only suggested otherwise.

## [1.8.0] - 2026-09-02

### Security

- The add-on no longer mounts the Home Assistant config directory or /ssl.
  Nothing in the add-on ever read either mount, but a custom Vector config
  (the `custom_config_path` option) could: a hostile file planted on /share
  could declare a `file` source for `.storage/` auth tokens, `secrets.yaml`
  or TLS private keys and an `http` sink to carry them off. Both mounts are
  gone and /share is now read-only.
- Vector's state directory (the journald read cursor) moved from
  `/share/vector` to the add-on's private `/data`. On /share, any add-on
  with a share mapping could rewrite the cursor to quietly suppress log
  shipping, or fill the directory to stop the shipper. The old state on
  /share is deliberately not imported for the same reason; the first start
  after this update re-reads the current boot's journal once, so a burst of
  already-shipped lines can reappear in VictoriaLogs. The leftover
  `/share/vector` folder is unused and safe to delete.

### Fixed

- `custom_config_path` under the add-on's own config folder works now. The
  documented `/addon_configs/...` form never could: that name only exists on
  the host, and the mount it refers to was shadowed by the (now removed)
  full-config mount. The folder is mounted at `/config`, so put the file
  there (host path `/addon_configs/<repo>_vector/`) and point
  `custom_config_path` at `/config/<file>.yaml`; `/share` paths keep
  working, read-only.

## [1.7.0] - 2026-08-28

### Changed

- Vector is now 0.58.0, up from 0.57.0. None of that release's breaking changes
  reach this add-on. It uses the journald source, a remap transform and the
  elasticsearch sink; 0.58 removed the azure_monitor_logs sink, the logdna sink
  alias, the http_server encoding option, the influxdb_logs namespace option and
  the legacy buffer metrics. The stricter rule on templated hostnames does not
  apply either, because the config generator substitutes the sink endpoint
  before Vector ever parses it.

## [1.6.2] - 2026-08-01

### Fixed

- Basic auth works again. 1.6.0 moved the credentials into the environment and
  had the generated config reference them as `${VICTORIALOGS_USER}` and
  `${VICTORIALOGS_PASSWORD}`, but Vector does not interpolate environment
  variables into a `--config-yaml` config. The sink authenticated with those
  literal strings, so a VictoriaLogs instance behind auth rejected every batch
  with 401 and dropped the events. Anyone who set a username and password has
  been shipping nothing since upgrading to 1.6.0; no configuration change is
  needed on your side, only this update
- The credentials now go through Vector's own secrets management. The config
  holds `SECRET[victorialogs.user]` and `SECRET[victorialogs.password]`, which
  Vector resolves at startup by running `vector-secrets.sh`. The password still
  never reaches disk and still cannot appear in the config dump printed on a
  validation failure

### Added

- The test suite now checks the `Authorization` header the sink actually sends,
  by running one Vector as the receiver and one as the sender. Every assertion
  in 1.6.0 passed while the add-on was authenticating with literal text, because
  none of them looked past the generated file
- A test that loads the generated config with Vector for real. `vector validate`
  does not resolve secrets, so the validation step the add-on already ran could
  not catch a substitution that breaks the config

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
