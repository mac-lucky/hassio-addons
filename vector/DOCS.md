# Home Assistant Add-on: Vector Log Collector

Vector is a high-performance, end-to-end observability data pipeline written in Rust.
This add-on collects logs from your Home Assistant system and sends them to VictoriaLogs.

## Features

- Collects systemd journal logs (Home Assistant Core, Supervisor, add-ons, host system)
- Collects Docker container logs
- Low memory footprint (~30-50MB RAM)
- Configurable filtering by unit/container name
- Custom labels for log enrichment
- Built-in configuration validation

## Installation

1. Add this repository to your Home Assistant Add-on Store
2. Install the Vector add-on
3. Configure the add-on with your VictoriaLogs endpoint
4. Start the add-on

## Configuration

### Required Options

| Option | Description |
|--------|-------------|
| `victorialogs_endpoint` | URL of your VictoriaLogs instance (e.g., `http://192.168.1.100:9428`) |

### Optional Options

| Option | Default | Description |
|--------|---------|-------------|
| `hostname` | System hostname | Override the hostname label |
| `instance` | `homeassistant` | Instance identifier for multi-HA setups |
| `log_level` | `info` | Logging verbosity (trace/debug/info/warning/error) |
| `collect_journal` | `true` | Collect systemd journal logs |
| `collect_docker` | `true` | Collect Docker container logs |
| `journal_include_units` | `[]` | Only collect from these systemd units |
| `journal_exclude_units` | `[]` | Exclude these systemd units |
| `docker_include_containers` | `[]` | Only collect from these containers |
| `docker_exclude_containers` | `[]` | Exclude these containers |
| `stream_fields` | `["host", "container_name", "unit"]` | Fields for VictoriaLogs stream identifiers |
| `extra_labels` | `{}` | Additional key-value labels to add to all logs |
| `custom_config_path` | `""` | Path to custom Vector config file (advanced) |

### Example Configuration

```yaml
victorialogs_endpoint: "http://192.168.1.100:9428"
hostname: "homeassistant-prod"
instance: "main"
log_level: "info"
collect_journal: true
collect_docker: true
journal_exclude_units:
  - "systemd-resolved"
  - "systemd-timesyncd"
docker_exclude_containers:
  - "addon_local_ssh"
extra_labels:
  environment: "production"
  location: "home"
```

## Log Sources

### Journal Logs

When `collect_journal` is enabled, the add-on collects all systemd journal entries including:

- **Home Assistant Core** logs
- **Supervisor** logs
- **Add-on** logs (via systemd units)
- **Host system** services

Use `journal_include_units` to collect only specific units, or `journal_exclude_units` to filter out noisy services.

### Docker Logs

When `collect_docker` is enabled, the add-on collects logs from Docker containers:

- **Add-on containers**
- **Home Assistant Core container**
- Any other containers running on the host

Use `docker_include_containers` or `docker_exclude_containers` to filter specific containers.

## VictoriaLogs Integration

Logs are sent to VictoriaLogs using the Elasticsearch-compatible bulk API:

- **Endpoint**: `{victorialogs_endpoint}/insert/elasticsearch/`
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

1. Create your Vector config file (e.g., `/share/vector/custom.yaml`)
2. Set `custom_config_path: "/share/vector/custom.yaml"`
3. The add-on will use your config instead of generating one

Your custom config must be valid Vector YAML configuration.

## Troubleshooting

### No logs appearing in VictoriaLogs

1. Check the add-on logs for errors
2. Verify the VictoriaLogs endpoint is reachable from Home Assistant
3. Ensure VictoriaLogs is accepting connections on the configured port
4. Check that at least one source (`collect_journal` or `collect_docker`) is enabled

### Configuration validation failed

The add-on validates the generated configuration before starting. If validation fails:

1. Check the add-on logs for the specific error
2. Review your configuration options
3. If using `custom_config_path`, validate your custom config with `vector validate`

### High memory usage

Vector typically uses 30-50MB of RAM. If usage is higher:

1. Reduce the number of collected sources
2. Add exclusions for noisy units/containers:

```yaml
journal_exclude_units:
  - "systemd-journald"
  - "systemd-timesyncd"
  - "systemd-resolved"
docker_exclude_containers:
  - "addon_local_ssh"
```

### Connection refused errors

If you see "connection refused" errors:

1. Verify VictoriaLogs is running and accessible
2. Check firewall rules allow connections from Home Assistant
3. Ensure the endpoint URL is correct (include `http://` or `https://`)

## Vector API

The add-on exposes Vector's API on port 8686 (optional). You can use this to:

- Check Vector's health: `http://<ha-ip>:8686/health`
- View metrics: `http://<ha-ip>:8686/metrics`

To expose the port, configure it in the add-on's network settings.

## Support

For issues and feature requests, please use the [GitHub issue tracker](https://github.com/mac-lucky/hassio-addons/issues).

## Resources

- [Vector Documentation](https://vector.dev/docs/)
- [VictoriaLogs Documentation](https://docs.victoriametrics.com/victorialogs/)
- [LogsQL Query Language](https://docs.victoriametrics.com/victorialogs/logsql/)
