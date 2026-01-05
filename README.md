# Mac Lucky's Home Assistant Add-ons

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A collection of Home Assistant add-ons focused on observability and monitoring.

## Add-ons

### Vector Log Collector

[![Vector CI](https://github.com/mac-lucky/hassio-addons/actions/workflows/ci.yaml/badge.svg)](https://github.com/mac-lucky/hassio-addons/actions/workflows/ci.yaml)

High-performance log collector that sends Home Assistant logs to VictoriaLogs.

- Collects systemd journal logs (HA Core, Supervisor, add-ons)
- Collects Docker container logs
- Low memory footprint (~30-50MB)
- Configurable filtering and labeling

## Installation

1. Add this repository to your Home Assistant Add-on Store:

   [![Add Repository](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Fmac-lucky%2Fhassio-addons)

   Or manually add the repository URL:
   ```
   https://github.com/mac-lucky/hassio-addons
   ```

2. Find the add-on in the Add-on Store and click Install

3. Configure the add-on with your VictoriaLogs endpoint

4. Start the add-on

## Support

For issues and feature requests, please use the [GitHub issue tracker](https://github.com/mac-lucky/hassio-addons/issues).

## License

MIT License - see [LICENSE](LICENSE) for details.
