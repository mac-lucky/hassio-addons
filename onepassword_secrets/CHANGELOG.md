# Changelog

All notable changes to this add-on are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

Nothing yet.

## [0.1.14] - 2026-11-22

### Fixed

- A change Home Assistant rejected after a wait no longer restarts Home
  Assistant unless it was actually seen down during that wait. A
  configuration check that only kept timing out while Home Assistant was up
  used to cost one Home Assistant restart. A change that replaces a waiting
  one, or that is written again after a hold for a stopped Home Assistant
  (once it answers, or with Sync now), keeps the mark, so a rejection still restarts Home Assistant onto the
  restored files when it may have started on the rejected ones.
- A restart Home Assistant refused, or one it did not go down for, no longer
  triggers a second restart after the rollback: Home Assistant is still
  running on the previous values, so only the integrations this change had
  reloaded are reloaded again, and the notification says which case it was.
- Stopping or updating the add-on while it restarts Home Assistant, or while
  it waits for the configuration check, no longer rolls the change back and
  holds it; the change stays pending and is finished on the next start.
- The configuration check runs once per change instead of twice back to back
  before a Home Assistant restart.
- An empty 1Password field no longer replaces the value a secrets file holds
  for that key (and the previous copy is no longer deleted after it). The
  file keeps its value until the field is filled, deleting the field leaves a
  hand-written value where it was, and a key that is new or already empty is
  still written empty, so a value left blank on purpose keeps working.
- When one add-on's options cannot be read, the keys it used last time still
  count as used by it: it is restarted when one of them changes, and a key it
  uses is not removed from `secrets.yaml` meanwhile. An add-on whose options
  were never read is asked again on the next sync.

## [0.1.13] - 2026-11-20

### Fixed

- A change that landed while Home Assistant was starting, restarting or
  updating was rolled back and held until Sync now, because the configuration
  check it could not answer counted as failed. The new files now stay in place
  and the check, reloads and restart run once Home Assistant answers again; a
  newer value that comes in the meantime is written and applied with it. Only
  an answer other than "valid" rolls a change back. If that answer comes
  after Home Assistant may have started with the change, it is restarted with
  the restored files (with `core_restart: notify`, a restart is asked for).
  If Home Assistant does not answer for 10 minutes, the change may be what
  stops it, so the previous files are restored and the change is tried again
  once Home Assistant answers.
- A change 0.1.12 held back this way is tried again after the update,
  without Sync now.

## [0.1.12] - 2026-11-12

### Changed

- fix(deps): update module go.yaml.in/yaml/v3 to v4 (#53)
- chore(deps): update all non-major dependencies (#52)
- fix(deps): update all non-major dependencies (#50)

### Security

- Rebuilt on the base image's current packages, picking up the fixes published
  after 0.1.11 was built.

## [0.1.11] - 2026-11-07

### Changed

- chore(deps): update ghcr.io/hassio-addons/debian-base docker tag to v9.5.2 (#45)

### Security

- Rebuilt on the base image's current packages, picking up the fixes published
  after 0.1.10 was built.

## [0.1.10] - 2026-10-30

### Changed

- chore(deps): update 1password/connect-api docker tag to v1.8.4 (#40)
- chore(deps): update all non-major dependencies (#35)
- chore(deps): update dependency go to v1.27.3 (#34)

### Security

- Rebuilt on the base image's current packages, picking up the fixes published
  after 0.1.9 was built.

## [0.1.9] - 2026-10-16

Rebuild only, no functional change.

### Security

- Rebuilt on the base image's current packages, picking up the fixes published
  after 0.1.8 was built.

## [0.1.8] - 2026-10-12

### Security

- Built with Go 1.27.2, which fixes GO-2026-6603, GO-2026-6605, GO-2026-6607,
  GO-2026-6608, GO-2026-6609, GO-2026-6610, GO-2026-6611, GO-2026-6612 and
  GO-2026-6613 in the standard library.

## [0.1.7] - 2026-10-11

### Changed

- The activity list says what a write did to the keys ("Added 23 keys",
  "Removed 1 key") instead of "Updated" for every change.

## [0.1.6] - 2026-10-11

### Fixed

- An error notification raised before the add-on restarted stayed in Home
  Assistant after the problem was gone. The first sync that ends healthy
  now dismisses it.

## [0.1.5] - 2026-10-11

### Changed

- A missing key that only ESPHome device files use is a warning instead of
  an error: the devices keep running, only their next install fails.
- For a missing key whose name 1Password cannot hold as a label (capital
  letters, dashes), the suggested fix says to rename it where it is used.

## [0.1.4] - 2026-10-11

### Fixed

- 0.1.3 still raised "Connect is not answering" on start: the first check
  ran before the Connect processes had a start time, so it did not count as
  starting.

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
