# 1Password Secrets

This add-on keeps Home Assistant's `secrets.yaml` (and, if you want, other
secrets files such as ESPHome's or Zigbee2MQTT's) filled from 1Password. It
runs its own 1Password Connect server inside the add-on, checks for changes
every minute, and when a value changes in 1Password it writes the new value
and makes whatever uses it pick it up: it reloads the integration, restarts
the add-on, or restarts Home Assistant behind a configuration check with an
automatic rollback.

It only ever reads from 1Password. Values never appear in its log, its panel,
its sensor or its events; the secrets files are the only place it writes them.

## How it works

1. The built-in Connect server keeps an encrypted local copy of the vaults it
   is allowed to read, so checking for changes is a local call with no rate
   limit.
2. Every field in those vaults whose label is a valid key becomes a secret of
   that name. There is no mapping to maintain: `wifi_password` in 1Password is
   `wifi_password` in `secrets.yaml`.
3. The add-on scans your configuration for `!secret` uses, so it knows which
   file each key belongs in and who has to act when it changes.
4. It edits each secrets file as YAML, not as text: keys from 1Password are
   updated, everything else in the file (hand-written keys, comments, order)
   stays.

## Setting it up

You need the 1Password CLI (`op`) signed in as an account owner or
administrator, once, to create the Connect server and its token.

1. Create a vault for Home Assistant and put your secrets in it (see the next
   section):

   ```sh
   op vault create homeassistant
   ```

2. Create a Connect server that can read it, and a read-only token for it:

   ```sh
   op connect server create homeassistant --vaults homeassistant
   op connect token create home-assistant --server homeassistant \
     --vault homeassistant,r --expires-in 365d
   ```

   The first command writes `1password-credentials.json` into the current
   directory. The second prints the token.

3. On the add-on's Configuration tab, paste the whole content of
   `1password-credentials.json` into **Connect credentials** and the token into
   **Connect access token**. Then delete the local credentials file.

4. Start the add-on and open the **Secrets** panel. A checklist shows each
   step until the first sync is done; the first sync of a large vault can take
   a minute.

Turn on **Dry run** first if you already have a `secrets.yaml`: the panel then
shows what would change in each file without writing anything.

## Putting secrets in 1Password

- **A field becomes a key** when its label is lowercase letters, digits and
  underscores (`mqtt_password`, `esp_kitchen_api_key`), and its type is
  password (concealed), text or URL. Put as many as you like in one item; group
  them however suits you.
- **Each key must come from one field.** If two fields in the vaults share a
  label, the key is reported as a conflict and keeps its last value until you
  rename one.
- **Skipped fields:** one-time passwords, notes, and the labels 1Password's own
  item templates use (`username`, `password`, `credential`, `hostname` and the
  like), so a Login item's default fields never turn into keys. Give a field
  its own label to use it.
- **Structured values:** a label ending in `_json` holds a JSON object or list,
  written into the secrets file as YAML structure. That is how a Google service
  account key works with `service_account: !secret google_assistant_service_account_json`,
  or Zigbee2MQTT's network key with `network_key: '!secret network_key_json'`.
- **Rotation dates:** optional text fields `rotated_at` (a date),
  `interval` (`365d`, `90d`, `on-compromise`) and `expires_at` on an item show
  up in the panel as a rotation bar, and keys past due are flagged.
- **Keys from elsewhere:** the **Extra references** option maps a key to any
  field the token can read, with an `op://vault/item/field` reference
  (`op://vault/item/section/field` for a field in a section). The token needs
  read access to that vault too.

## Which file gets which key

**Secrets files** lists the files the add-on writes, relative to the config
directory, each named `secrets.yaml` or `secret.yaml` (the names Home
Assistant, ESPHome and Zigbee2MQTT read secrets from). `secrets.yaml` is always
one of them.

- `secrets.yaml` gets every key. Supervisor reads add-on options' `!secret`
  values from it, and a new key is in place before the configuration that uses
  it is.
- Another file gets the keys that the configuration next to it uses:
  `esphome/secrets.yaml` gets what the ESPHome device files refer to with
  `!secret`, `zigbee2mqtt/secret.yaml` gets what Zigbee2MQTT's configuration
  refers to with `'!secret key'`. Home Assistant and ESPHome look for the
  nearest `secrets.yaml` from the file that uses a key, and the add-on follows
  the same rule.

Keys in a file that are not in 1Password are left alone and shown as "only in
file". A key the add-on wrote and that later disappears from 1Password is
removed again, unless something still uses it; then it stays and the panel
tells you.

## When a value changes

The add-on applies a change in this order, and records each step in the panel's
activity list:

1. It writes the files, keeping a copy of each previous version.
2. If an integration configured in YAML uses the key, it runs Home Assistant's
   configuration check, then calls that integration's `reload` service where
   there is one.
3. If an integration that cannot reload uses it, and **Home Assistant restart**
   is `auto`, it restarts Home Assistant and waits for it to answer again. With
   `notify`, it leaves a notification and a **Restart Home Assistant** button
   in the panel instead.
4. It restarts the add-ons whose options use the key (`!secret key`, or the
   GitOps agent's `secret://key`) and add-ons that read a changed file in their
   own directory (Zigbee2MQTT for `zigbee2mqtt/secret.yaml`). Supervisor only
   rereads `secrets.yaml` once a minute, so this waits about a minute after the
   write.
5. ESPHome devices are compiled with their secrets, so for those it only tells
   you which devices to install again.
6. It fires the `onepassword_secrets_changed` event.

If the configuration check fails, or Home Assistant does not come back after
the restart, the previous files are restored (and Home Assistant restarted
again), and the same change is not tried again until you press **Sync now** or
the value changes in 1Password again.

## Moving an existing secrets.yaml to 1Password

1. Turn on **Dry run** and start the add-on.
2. For each key in your `secrets.yaml`, add a field with the same label and
   value in the vault. The panel shows each key you have done as "in sync" and
   each one still to do as "only in file".
3. When the dry run shows nothing would change, turn **Dry run** off. The first
   write only adds a header comment.
4. From then on, change values in 1Password, not in the file.

If another tool writes the same file (for example a GitOps setup that decrypts
it from git), stop that first; two writers will keep undoing each other. The
GitOps Agent add-on in this repository never touches secrets files since
version 0.9.0.

## Options

| Option | Default | What it does |
| --- | --- | --- |
| `connect_credentials` | | `1password-credentials.json` of this add-on's Connect server, raw or base64 |
| `connect_token` | | Read-only access token for that server |
| `connect_url` | | An external Connect server to use instead of the built-in one |
| `vaults` | `homeassistant` | Vaults whose fields become keys |
| `references` | | Extra `key` + `ref` pairs |
| `secrets_files` | `secrets.yaml` | Files to write |
| `poll_interval_seconds` | `60` | How often to check for changes (15-3600) |
| `restart_addons` | `true` | Restart add-ons that use a changed key |
| `core_restart` | `auto` | `auto` or `notify` for a change that needs a Home Assistant restart |
| `dry_run` | `false` | Show changes without writing anything |
| `log_level` | `info` | `debug`, `info`, `warning` or `error` |

## In Home Assistant

- **Sensor** `sensor.onepassword_secrets`: `ok`, `attention`, `error`,
  `starting` or `unconfigured`, with the number of managed, missing and
  unmanaged keys, keys due for rotation, whether a restart is pending, and the
  days until the access token expires.
- **Event** `onepassword_secrets_changed`, with `keys`, `files`, `reloaded`,
  `restarted_addons` and `core_restarted`. Key names only.
- **Sync on demand** from an automation:

  ```yaml
  action: hassio.addon_stdin
  data:
    addon: <this add-on's slug>
    input: sync
  ```

- **Notifications** for a failed or rolled back change, a pending restart,
  ESPHome devices to install again, and a token that expires within 30 days.

## What is stored where

- The Connect credentials and token are add-on options. Supervisor shows add-on
  options to other add-ons that have the manager role, and they are in Home
  Assistant backups. Scope the token to read-only access on the vaults Home
  Assistant needs, and nothing else.
- The add-on writes the credentials to `/data/connect/1password-credentials.json`
  (mode 0600) for the Connect processes, which keep their encrypted copy of the
  vaults next to it. Neither process sees the Supervisor token.
- Connect cannot be told to listen on localhost only, so its ports (8080, 8081
  and the bus ports 11220 and 11221) are reachable from other add-ons and Home
  Assistant on the internal hassio network, the same as in 1Password's own
  Docker and Kubernetes deployments. None is published on the host, and the API
  answers nothing without the access token.
- `/data/state.json` records which keys the add-on wrote to which file, and a
  fingerprint of each value: an HMAC with a random key generated on first start
  (`/data/fingerprint.key`), so the state never holds a value or a plain hash
  of one.
- While a change is being applied, the previous version of each file is kept
  under `/data/previous` (mode 0600) and deleted once the change is through.
- The secrets files themselves are written atomically, keeping their file mode
  (0600 for a new file).
- The panel is reachable only through Home Assistant's ingress, and refuses
  cross-site form posts.

## Rotating the access token

Tokens made with `--expires-in 365d` expire after a year; the panel, the sensor
and a notification warn 30 days ahead.

```sh
op connect token create home-assistant-2027 --server homeassistant \
  --vault homeassistant,r --expires-in 365d
```

Put the new token in **Connect access token**, restart the add-on, check that
the panel is in sync, then revoke the old token:

```sh
op connect token list --server homeassistant
op connect token delete <old token id> --server homeassistant
```

## Troubleshooting

- **"Waiting for Connect's first sync"** stays: the credentials do not belong
  to a Connect server, or the server was deleted. The add-on log shows the
  Connect processes' own messages.
- **"Connect refused the access token"**: the token expired, was revoked, or
  belongs to another Connect server.
- **"Vault is not readable"**: the server or the token was not given that
  vault. Tokens cannot be widened; create a new one.
- **A key is "missing"**: something uses `!secret key` but there is no field
  with that label and no such key in the file.
- **Nothing reloads after a change**: the key is only used by files Home
  Assistant does not load from `configuration.yaml`; the activity list names
  the file.
