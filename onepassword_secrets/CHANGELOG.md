# Changelog

All notable changes to this add-on are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

Nothing yet.

## [0.1.3] - 2026-10-11

### Fixed

- Every start raised a "Connect is not answering" error and notification
  for a minute, because the first check ran before Connect was listening.
  For a minute after a Connect process starts, the add-on now shows
  "Starting Connect" and checks again every 10 seconds.
- Configurations of devices archived in the ESPHome dashboard
  (`esphome/archive`) were scanned, so their old `!secret` uses showed up as
  missing keys.

## [0.1.2] - 2026-10-11

### Fixed

- The add-on stayed on "Waiting for Connect's first sync" after Connect had
  synced: Connect 1.8.3 reports its account data as `AVAILABLE`, and only
  `ACTIVE` was taken as synced.

## [0.1.1] - 2026-10-11

### Fixed

- The built-in Connect server did not start on Home Assistant OS: the
  add-on's AppArmor profile kept Connect from reading `/etc/passwd`, which it
  needs to check who owns its data directory, so both processes exited
  straight away and the panel showed "Connect is not answering".

### Changed

- When the Connect processes keep exiting, the panel and sensor now say
  "Connect keeps stopping" and point to the add-on log, instead of
  "Connect is not answering".

## [0.1.0] - 2026-10-10

First release.

### Added

- Built-in 1Password Connect server (1.8.3), started and supervised by the
  add-on; an external Connect server can be used instead.
- Every field in the configured vaults whose label is a secrets key becomes
  that key; labels ending in `_json` are written as YAML structure. Extra keys
  can point anywhere with `op://` references.
- Writes `secrets.yaml` and any other configured secrets file (ESPHome's,
  Zigbee2MQTT's), editing them as YAML so hand-written keys and comments stay.
- Follows changes: reloads the YAML integrations that use a changed key,
  restarts add-ons that use it, and restarts Home Assistant after a
  configuration check when an integration cannot reload, restoring the previous
  files if it does not come back.
- Panel with every key, its source, what uses it and its rotation due date;
  dry run; the `sensor.onepassword_secrets` sensor, the
  `onepassword_secrets_changed` event and `hassio.addon_stdin` sync.
