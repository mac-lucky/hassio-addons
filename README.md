<img src="icon.svg" width="96" align="right" alt="">

# Mac Lucky's Home Assistant Add-ons

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A collection of Home Assistant add-ons focused on observability and configuration management.

## Add-ons

### Vector Log Collector

[![CI](https://github.com/mac-lucky/hassio-addons/actions/workflows/ci.yaml/badge.svg)](https://github.com/mac-lucky/hassio-addons/actions/workflows/ci.yaml)

High-performance log collector that sends Home Assistant logs to VictoriaLogs.

- Collects systemd journal logs (HA Core, Supervisor, add-ons, host services)
- Low memory footprint (~30-50MB)
- Configurable filtering and labeling

### GitOps Agent

[![GitOps Agent CI](https://github.com/mac-lucky/hassio-addons/actions/workflows/ci-gitops-agent.yaml/badge.svg)](https://github.com/mac-lucky/hassio-addons/actions/workflows/ci-gitops-agent.yaml)

Keeps `/config` in sync with a git repository, the way Flux or Argo CD keeps a
cluster in sync.

- Dry-run diff, validate-then-apply, and rollback if Home Assistant refuses the result
- Optionally reconciles floors, areas, labels, helpers, entity customizations,
  dashboards, other add-ons' options, config-flow integrations and HACS installs
- Never touches secrets files: pair it with 1Password Secrets (below), or
  anything else that writes `secrets.yaml`
- Syncs live edits back to the tracked branch or to a review branch, seeds a
  repository from an existing config, and can be triggered by webhook instead
  of only polling

### 1Password Secrets

[![1Password Secrets CI](https://github.com/mac-lucky/hassio-addons/actions/workflows/ci-onepassword-secrets.yaml/badge.svg)](https://github.com/mac-lucky/hassio-addons/actions/workflows/ci-onepassword-secrets.yaml)

Keeps `secrets.yaml` (and ESPHome's or Zigbee2MQTT's secrets files) filled
from 1Password through a built-in Connect server.

- A field labelled `wifi_password` is the secret `wifi_password`; no mapping to
  maintain
- Follows changes in 1Password: reloads, restarts add-ons, or restarts Home
  Assistant behind a configuration check with rollback
- Panel with each key's source, users and rotation date; values are never shown

## Installation

1. Add this repository to your Home Assistant Add-on Store:

   [![Add Repository](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Fmac-lucky%2Fhassio-addons)

   Or manually add the repository URL:
   ```
   https://github.com/mac-lucky/hassio-addons
   ```

2. Find the add-on in the Add-on Store and click Install

3. Configure it on the add-on's Configuration tab - a VictoriaLogs endpoint for
   Vector, a repository URL for the GitOps Agent

4. Start the add-on

## Support

For issues and feature requests, please use the [GitHub issue tracker](https://github.com/mac-lucky/hassio-addons/issues).

## License

MIT License - see [LICENSE](LICENSE) for details.
