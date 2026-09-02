# Home Assistant Add-on: Vector Log Collector

Vector is a high-performance, end-to-end observability data pipeline written in Rust.
This add-on collects logs from your Home Assistant system and sends them to VictoriaLogs.

## Features

- Collects systemd journal logs (Home Assistant Core, Supervisor, add-ons, host system)
- Low memory footprint (~30-50MB RAM)
- Configurable filtering by systemd unit
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
| `hostname` | System hostname | Override the hostname label |
| `instance` | `homeassistant` | Instance identifier for multi-HA setups |
| `log_level` | `info` | Logging verbosity (trace/debug/info/warning/error) |
| `collect_journal` | `true` | Collect systemd journal logs |
| `redact_sensitive` | `true` | Replace API keys, tokens and passwords found in log messages with `[REDACTED]` |
| `journal_include_units` | `[]` | Only collect from these systemd units |
| `journal_exclude_units` | `[]` | Exclude these systemd units |
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

When `collect_journal` is enabled, the add-on collects all systemd journal entries including:

- **Home Assistant Core** logs
- **Supervisor** logs
- **Add-on** logs (via systemd units)
- **Host system** services

Use `journal_include_units` to collect only specific units, or `journal_exclude_units` to filter out noisy services.

## VictoriaLogs Integration

Logs are sent to VictoriaLogs using the Elasticsearch-compatible bulk API:

- **Endpoint**: `{victorialogs_endpoint}` with `/_bulk` appended by Vector
- **Compression**: gzip
- **API version**: v8

### Stream Fields

The `stream_fields` option controls how logs are grouped in VictoriaLogs. The default fields are:

- `host` - The hostname of your Home Assistant instance
- `container_name` - Name of the container/service
- `unit` - Systemd unit name

### Querying Logs in VictoriaLogs

Once running, you can query your logs using LogsQL:

```logsql
# All Home Assistant logs
{instance="homeassistant"}

# Logs from a specific container
{container_name="homeassistant"}

# Logs from a specific host
{host="homeassistant-prod"}

# Error level logs
{instance="homeassistant"} level:error

# Search for specific text
{instance="homeassistant"} "error connecting"
```

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

None of the other options apply while a custom config is in use. That includes
the basic auth credentials: `/share` is writable by every add-on that maps it,
and a custom config could declare the same secret backend and read the password
back out, so the backend serves nothing in this mode. Put the credentials
directly in your own config, or use Vector's own secrets support.

If a custom config fails validation the add-on will not print it. Run
`vector validate` against your file to see the details.

## Troubleshooting

### No logs appearing in VictoriaLogs

1. Check the add-on logs for errors
2. Verify the VictoriaLogs endpoint is reachable from Home Assistant, including the insert path
3. Check that VictoriaLogs is accepting connections on the configured port
4. Check that `collect_journal` is enabled

### Configuration validation failed

The add-on validates the generated configuration before starting. If validation fails:

1. Check the add-on logs for the specific error
2. Review your configuration options
3. If using `custom_config_path`, validate your custom config with `vector validate`

On failure the add-on prints the generated configuration with credentials
redacted, so it is safe to paste into a bug report.

### High memory usage

Vector typically uses 30-50MB of RAM. If usage is higher, add exclusions for
noisy units:

```yaml
journal_exclude_units:
  - "systemd-journald.service"
  - "systemd-timesyncd.service"
  - "systemd-resolved.service"
```

### Connection refused errors

If you see "connection refused" errors:

1. Verify VictoriaLogs is running and accessible
2. Check firewall rules allow connections from Home Assistant
3. Ensure the endpoint URL is correct (include `http://` or `https://`)

## Vector API

Vector's API is enabled but bound to `127.0.0.1:8686` inside the container, so
it is not reachable from the network even if you map the port. It is there for
`vector top` and similar commands run from an add-on shell.

## Support

For issues and feature requests, please use the [GitHub issue tracker](https://github.com/mac-lucky/hassio-addons/issues).

## Resources

- [Vector Documentation](https://vector.dev/docs/)
- [VictoriaLogs Documentation](https://docs.victoriametrics.com/victorialogs/)
- [LogsQL Query Language](https://docs.victoriametrics.com/victorialogs/logsql/)
