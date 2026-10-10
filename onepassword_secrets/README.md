# Home Assistant Add-on: 1Password Secrets

Keeps Home Assistant's secrets files in sync with 1Password through a built-in
1Password Connect server, and applies changed values by itself.

![Supports aarch64 Architecture][aarch64-shield]
![Supports amd64 Architecture][amd64-shield]

- A field whose label is a secrets key (`wifi_password`) is that key; no
  mapping to maintain.
- Writes `secrets.yaml` and, optionally, ESPHome's and Zigbee2MQTT's secrets
  files, editing them as YAML so hand-written keys and comments stay.
- When a value changes in 1Password: reloads the integration that uses it,
  restarts add-ons that use it, or restarts Home Assistant after a configuration
  check, rolling back if it does not come back.
- A panel showing every key, where it comes from, what uses it and when it is
  due for rotation. Values are never shown, logged or sent anywhere but the
  secrets files.

See [DOCS.md](DOCS.md) for setup and options.

## Installation

1. Add this repository to Home Assistant: **Settings > Add-ons > Add-on Store >
   menu > Repositories**, `https://github.com/mac-lucky/hassio-addons`.
2. Install **1Password Secrets**.
3. Follow [DOCS.md](DOCS.md) to create the Connect server and token, and paste
   them into the Configuration tab.

## Development

The add-on is one Go module in this directory:

```sh
go build ./... && go vet ./... && gofmt -l .
go test -race ./...
go test -race -tags dev ./internal/web/
golangci-lint run
```

The panel has previews of each state with fake data, no Connect server needed:

```sh
OPS_DEV=1 go run -tags dev ./cmd/onepassword-secrets
# http://localhost:8099/?preview=healthy (also firstrun, starting,
# attention, restart, error, dryrun)
```

[aarch64-shield]: https://img.shields.io/badge/aarch64-yes-green.svg
[amd64-shield]: https://img.shields.io/badge/amd64-yes-green.svg
