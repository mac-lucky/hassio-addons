# Home Assistant Add-on: Vector Log Collector

Vector is a high-performance, end-to-end observability data pipeline written in Rust.
This add-on collects logs from your Home Assistant system and sends them to VictoriaLogs.

## Features

- Collects systemd journal logs (Home Assistant Core, Supervisor, add-ons, host system)
- Low memory footprint (~30-50MB RAM)
- Filtering by add-on container or systemd unit
- Python tracebacks and other multi-line output joined into one entry
- Colour codes stripped and log levels read from the line itself
- Custom labels for log enrichment
- Optional redaction of secrets found in log messages
- Built-in configuration validation

Docker container logs are not collected separately. On Home Assistant OS the
journal already carries container output, so the journald source covers it.

## Installation

1. Add this repository to your Home Assistant Add-on Store
2. Install the Vector add-on
3. Configure the add-on with your VictoriaLogs endpoint
4. Start the add-on

## Configuration

### Required Options

| Option | Description |
|--------|-------------|
| `victorialogs_endpoint` | Full URL of the VictoriaLogs insert path (e.g., `http://192.168.1.100:9428/insert/elasticsearch`) |

The endpoint is passed to Vector's `elasticsearch` sink verbatim and Vector
appends `/_bulk`, so the insert path has to be part of the URL. A bare
`http://192.168.1.100:9428` will not work.

### Optional Options

| Option | Default | Description |
|--------|---------|-------------|
| `victorialogs_username` | `""` | Username for basic auth (leave empty to disable) |
| `victorialogs_password` | `""` | Password for basic auth, only used when a username is set |
| `hostname` | From the journal | Override the `host` label |
| `instance` | `homeassistant` | Instance identifier for multi-HA setups |
| `log_level` | `info` | Logging verbosity (trace/debug/info/warning/error) |
| `collect_journal` | `true` | Collect systemd journal logs |
| `redact_sensitive` | `true` | Replace API keys, tokens, passwords and known provider credential formats found in log messages with `[REDACTED]` |
| `journal_include_units` | `[]` | Only collect from these systemd units |
| `journal_exclude_units` | `[]` | Exclude these systemd units |
| `include_containers` | `[]` | Only collect these containers; Core and the Supervisor count (see [Containers](#containers)) |
| `exclude_containers` | `[]` | Never collect these add-on containers |
| `multiline_containers` | `["homeassistant", "hassio_supervisor"]` | Join multi-line output from these containers into one entry |
| `stream_fields` | `["host", "container_name", "unit"]` | Fields for VictoriaLogs stream identifiers |
| `extra_labels` | `{}` | Additional key-value labels to add to all logs |
| `custom_config_path` | `""` | Path to custom Vector config file (advanced) |

### Example Configuration

```yaml
victorialogs_endpoint: "http://192.168.1.100:9428/insert/elasticsearch"
victorialogs_username: "myuser"
victorialogs_password: "mypassword"
hostname: "homeassistant-prod"
instance: "main"
log_level: "info"
collect_journal: true
redact_sensitive: true
journal_exclude_units:
  - "systemd-resolved.service"
  - "systemd-timesyncd.service"
exclude_containers:
  - "wmbusmeters-ha-addon"
multiline_containers:
  - "homeassistant"
  - "hassio_supervisor"
  - "music_assistant"
extra_labels:
  environment: "production"
  location: "home"
```

The credentials are never written into the generated Vector configuration. It
holds `SECRET[victorialogs.user]` and `SECRET[victorialogs.password]`, which
Vector resolves at startup by running a helper that reads them from the add-on
options, so they cannot appear in the add-on log if the configuration fails to
validate.

## Log Sources

### Journal Logs

When `collect_journal` is enabled, the add-on collects every systemd journal
entry on the host:

- **Home Assistant Core**, the **Supervisor**, its plugins and every
  **add-on**, all of which run as Docker containers
- **Host system** services, the kernel and the audit log

Kernel and audit entries have no systemd unit, so they are labelled after
their source instead (`unit` and `container_name` are `kernel` or `audit`)
and get a stream of their own.

### Containers

On Home Assistant OS every container logs through `docker.service`, so
`journal_include_units` and `journal_exclude_units` cannot tell one add-on from
another. Use `include_containers` and `exclude_containers` for that, and keep
the unit options for host services.

An entry matches a container that has exactly that name, or whose name ends
in `_<entry>`. `music_assistant` therefore matches
`app_d5369777_music_assistant`, and it keeps matching if the Supervisor
renames the container prefix. The names to use are the `container_name`
values you already see in VictoriaLogs; Core is `homeassistant` and the
Supervisor is `hassio_supervisor`. Only letters, digits, `_` and `-` are
allowed.

- `exclude_containers` drops everything those containers log.
- `include_containers`, when not empty, drops every container that is not on
  the list. Host services, the kernel and the audit log are not containers and
  are left to the unit options.

### Multi-line entries

The journald Docker driver stamps every stderr line `PRIORITY=3`, so each
frame of a Python traceback would otherwise arrive as its own `error` entry.
For the containers in `multiline_containers`, a line that does not open a new
log record is merged into the one before it, giving one entry per error with
the stack attached and the level taken from the opening line. A line opens a
new record when it starts with a date and time, a bracketed time such as
`[12:00:00]`, a level word such as `INFO` or `ERROR`, or `s6-rc:`.

Add a container here only if its log lines start that way. Otherwise every
line looks like a continuation, and lines are merged until one of the caps
below is reached.

`homeassistant` and `hassio_supervisor` are on the list by default. Other
Python-based add-ons such as `music_assistant`, `appdaemon` or `esphome`
usually benefit too, and so do `aircast` and `airsonos`, whose AirPlay
metadata dumps span several lines. Set the option to `[]` to turn joining off. Because a
merged entry is only complete once the next log line arrives, an isolated
error from these containers can reach VictoriaLogs up to about three seconds
later. A merged entry is capped at 200 lines or ten seconds, whichever comes
first.

Docker also splits any single line longer than 16 KB into pieces. The add-on
always puts those back together before anything else happens to them, so a
line of up to 96 KB arrives as one entry; a longer one arrives as several.

### What happens to each entry

- **Level.** The level word in the line itself wins over the journal
  priority, since Docker marks all of stderr as an error. The formats that are
  recognised are:
  - Home Assistant style `2026-01-11 09:04:15 WARNING`
  - ISO timestamps as written by Vector, Go and Rust programs, and
    cloudflared's `INF`/`WRN`/`ERR`
  - zigbee2mqtt's `[2026-01-11 09:04:15] error:`
  - bashio's `[09:04:15] INFO:`
  - `s6-rc: info:`
  - a bare level word opening the line, as esphome writes `INFO Reading ...`
  - logfmt `level=error`

  The level is one of `debug`, `info`, `notice`, `warn` and `error`; critical
  and fatal count as `error`. An entry with no priority at all (the audit log)
  is `info`, or `warn` for an AppArmor denial. Two programs print no level
  word and write everything to stderr, so some of their lines are `info`
  rather than `error`:
  - AirConnect (the AirCast and AirSonos add-ons), recognised by its
    `[08:10:05.030] AddCastDevice:673 ...` format in any container. A line
    that says something like `cannot`, `unable`, `failed` or `error` stays
    `error`, since its real errors look the same as the rest.
  - sshd in the SSH add-on, for its connection and login lines.
- **Colour codes** and trailing whitespace, such as the carriage return sshd
  ends its lines with, are removed from the message.
- **Blank lines** are dropped.
- **Very long messages** over 200 KB are cut to 50,000 characters. VictoriaLogs
  silently drops entries over 256 KB.
- **Redaction.** With `redact_sensitive` on, the message is scrubbed of:
  - authorization headers and API keys;
  - tokens, passwords and secrets written as `key: value` or `key=value`;
  - command-line flags such as `--password x`;
  - credentials inside URLs;
  - bare JWTs, which is what Home Assistant access tokens are;
  - about 400 provider credential formats (GitHub, GitLab, AWS, Slack, Stripe,
    OpenAI and many more), taken from the betterleaks rule set. These replace
    only the credential itself, with `[REDACTED:<rule-id>]`, so the rest of the
    line stays readable.

  Keys and values may be quoted, including JSON logged inside JSON. The process
  command line (`_CMDLINE`) and journald's raw copy of a rewritten message
  (`SYSLOG_RAW`) are dropped as well, since they can carry credentials too.

## VictoriaLogs Integration

Logs are sent to VictoriaLogs using the Elasticsearch-compatible bulk API:

- **Endpoint**: `{victorialogs_endpoint}` with `/_bulk` appended by Vector
- **Compression**: gzip
- **API version**: v8
- **Delivery**: acknowledged. The journal position only moves past an entry
  once VictoriaLogs has accepted it, so a restart while VictoriaLogs is down
  resends what was in flight rather than losing it. The price is that such a
  restart can store an entry twice.

### Stream Fields

The `stream_fields` option controls how logs are grouped in VictoriaLogs. The default fields are:

- `host` - The hostname of your Home Assistant instance
- `container_name` - Name of the container/service
- `unit` - Systemd unit name

### Querying Logs in VictoriaLogs

Once running, you can query your logs using LogsQL:

```logsql
# All logs from this Home Assistant instance
instance:=homeassistant

# Logs from a specific container (a stream field, so {} works)
{container_name="homeassistant"}

# Logs from a specific host
{host="homeassistant-prod"}

# Errors from every container, counted
instance:=homeassistant level:=error | stats by (container_name) count()

# Search for specific text
instance:=homeassistant "error connecting"
```

`{...}` filters only work on stream fields (`stream_fields`, by default
`host`, `container_name` and `unit`). `instance`, `level` and your extra labels
are ordinary fields, so filter them with `field:=value` instead.

## Advanced: Custom Configuration

For advanced users, you can provide a complete custom Vector configuration:

1. Create your Vector config file in this add-on's config folder - on the host
   that is `/addon_configs/<repo>_vector/custom.yaml` (visible in the Samba /
   VSCode "addon_configs" share) - or under `/share`
2. Set `custom_config_path: "/config/custom.yaml"` (the add-on sees its own
   config folder at `/config`) or `custom_config_path: "/share/vector/custom.yaml"`
3. The add-on will use your config instead of generating one

The path is checked inside the add-on container, so it must start with
`/config/` or `/share/`, and the file must be valid Vector YAML. The add-on's
config folder is the better home: `/share` is writable by every add-on that
maps it. If the path does not exist the add-on refuses to start rather than
falling back to the generated configuration, so a typo cannot leave you running
a config you did not intend.

None of the other options apply while a custom config is in use, and none of
them are validated - `victorialogs_endpoint` may be left empty. That includes
the basic auth credentials: `/share` is writable by every add-on that maps it,
and a custom config could declare the same secret backend and read the password
back out, so the backend serves nothing in this mode. Put the credentials
directly in your own config, or use Vector's own secrets support.

Set `data_dir: /data/vector` in your config so Vector's state (the journald
cursor, disk buffers) survives restarts; without it Vector falls back to
`/var/lib/vector`, which is recreated empty on every container start.

If a custom config fails validation the add-on will not print it. Run
`vector validate` against your file to see the details.

## Troubleshooting

### No logs appearing in VictoriaLogs

1. Check the add-on logs for errors
2. Verify the VictoriaLogs endpoint is reachable from Home Assistant, including the insert path
3. Check that VictoriaLogs is accepting connections on the configured port
4. Check that `collect_journal` is enabled

### Configuration validation failed

The add-on validates the generated configuration before starting. If validation
fails, or an option is invalid, the add-on stops with the reason at the end of
its log instead of restarting. To fix it:

1. Check the add-on logs for the specific error
2. Review your configuration options
3. If using `custom_config_path`, validate your custom config with `vector validate`

On failure the add-on prints the generated configuration with credentials
redacted, so it is safe to paste into a bug report.

### High memory usage or noisy logs

Vector typically uses 30-50MB of RAM. If usage is higher, or one add-on floods
the logs, exclude it by container, and noisy host services by unit:

```yaml
exclude_containers:
  - "wmbusmeters-ha-addon"
journal_exclude_units:
  - "systemd-timesyncd.service"
```

### Connection refused errors

If you see "connection refused" errors:

1. Verify VictoriaLogs is running and accessible
2. Check firewall rules allow connections from Home Assistant
3. Check the endpoint URL (include `http://` or `https://`)

## Vector API

Vector's API is enabled but bound to `127.0.0.1:8686` inside the container, so
it is not reachable from the network. It is there for `vector top` and similar
commands run from an add-on shell.

## Support

For issues and feature requests, please use the [GitHub issue tracker](https://github.com/mac-lucky/hassio-addons/issues).

## Resources

- [Vector Documentation](https://vector.dev/docs/)
- [VictoriaLogs Documentation](https://docs.victoriametrics.com/victorialogs/)
- [LogsQL Query Language](https://docs.victoriametrics.com/victorialogs/logsql/)
