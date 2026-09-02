# Documentation

## What it does

GitOps Agent maintains a clone of a git repository in its own data
directory (`/data/repo`) and reconciles selected files from that clone
into your live `/homeassistant` directory (this add-on's mount of your
Home Assistant config, exposed inside the container as `/homeassistant`
rather than `/config`). Every run computes a diff first; whether that
diff is applied automatically or only surfaced for review is
controlled by the `dry_run` option.

The agent never runs `git` inside `/homeassistant`. Your Home Assistant
config directory is not, and does not need to be, a git working tree.

## Options

### `repo_url`

HTTPS URL of the git repository holding your Home Assistant config,
e.g. `https://github.com/you/ha-config.git`. Required.

### `branch`

Branch to track. Default `main`. The agent only ever fast-forwards to
the tip of this branch, and never force-updates your repo. Three
operations write to it, each behind its own option and all off by
default: the manual Import button (`allow_import`), the add-on version
record (`track_addon_versions`), and capturing live edits
(`capture_live_changes`). Every one of those pushes is fast-forward only
and never forced, so a branch that moved on the remote is never
clobbered - the push is refused instead, and the agent says so.

### `git_username`

Username for HTTPS authentication, if the repository is private.
Leave empty for public repositories or when using a token that does
not require one (e.g. a fine-grained PAT used alone).

### `git_token`

Personal access token or app password for HTTPS authentication.
Stored only in the add-on's options (`/data/options.json`, managed by
Supervisor) and never written into the cloned repository or logged.
The local clone itself is anonymous - it is checked out from a plain,
credential-free remote URL, so the token never lands in `.git/config`
or anywhere else on disk under the clone. Each fetch instead supplies
it as an HTTP Basic `Authorization` header, passed through a one-shot
environment variable scoped to that single git subprocess - never as
part of the command line, and never persisted to any git config file
or credential helper. The residual exposure is narrower as a result:
for the few moments one fetch is actually running, another process
running as the same user could in principle read its
`/proc/<pid>/environ`, but nothing durable ever carries the token.
Any error message or log line that would otherwise echo it back - the
raw token or its base64-encoded form - has it redacted first.

The options store itself is the weakest link: `format: password` only
masks the UI, and Supervisor's API hands the whole options object back
in the clear to anything holding a hassio-API token - other add-ons,
diagnostic tools, support bundles. To keep the token off that surface,
set the option to `secret://<name>` instead of the literal value and
put the real token under that key in your live
`/homeassistant/secrets.yaml`, the same file Home Assistant's own
`!secret` reads. The reference is resolved once at startup; a name
that does not resolve stops the add-on instead of being used
literally, and a changed secret is picked up on the next add-on
restart. `age_key` and `webhook_secret` accept the same form.

### `interval_minutes`

How often (1-1440 minutes) the agent fetches the remote branch and
reconciles. Default 5.

### `dry_run`

When `true` (default), the agent computes and surfaces diffs but never
writes to `/homeassistant` on its own; you apply changes manually from the
web UI. When `false`, changes that pass validation are applied
automatically on every reconcile.

**Start with `dry_run: true`.** Only turn it off once you trust the
diff output for your setup.

### `commit_back`

When `true` (default `false`) **and** `dry_run` is also on, every
reconcile that finds live file drift pushes it automatically, with no
button click and no confirmation - to a new
`gitops/drift-<timestamp>` branch, on its own, as soon as it is found,
instead of only ever showing it in the web UI until the next apply
discards it. **`git_token` must have push rights to the repository for
this to work** - with `dry_run` off, `commit_back` only enables the
manual "commit drift back" button instead of pushing on its own. See
"Drift commit-back" below for exactly what gets captured and when.

### `allow_import`

Default `false`. Enables the two import buttons in the web UI:
**Preview**, which reports what a full snapshot of your live
config would capture without running a single git command, and
**Import** (from Home Assistant), which pushes that snapshot as one
commit directly onto the branch named in `branch`.

This is the only thing the add-on ever writes to your tracked branch,
so it is off by default, never runs on the interval, and only ever
happens on a button click. **`git_token` must have push rights to that
branch.** See "Importing an existing config" below for exactly what is
and is not captured.

Turn it on to seed a repository, run the import, then turn it back
off - nothing about ordinary syncing needs it.

### `webhook_secret`

Empty by default (the listener never starts). Set it to a shared
secret of at least 16 characters to enable a small separate
`POST /webhook` listener on port 8098 that triggers an immediate
reconcile - useful for a git host's push webhook instead of waiting
for the next `interval_minutes` tick. A shorter secret is refused at
startup and the listener stays off. See "Webhook trigger" below.

### `apply_after_pull`

What to do after a successful apply:

- `reload` (default): call `homeassistant.reload_all`, which reloads
  YAML-based config (automations, scripts, scenes, etc.) without a
  full restart.
- `restart`: fully restart Home Assistant core. Needed for changes
  that reload cannot pick up (e.g. some integration config).
- `off`: apply files but do not reload or restart anything; you
  trigger that yourself.

### `age_key`

Empty by default, which keeps secrets out of git entirely - the
behavior every earlier version had, and still the right choice if you
do not want secret material in your repository at all.

Set it to an age private key (`AGE-SECRET-KEY-1...`) and the agent
encrypts secret values with SOPS before they are pushed, then decrypts
them again when applying. `secrets.yaml` stops being an excluded file
and becomes an ordinary synced one. See "Secret encryption (SOPS and
age)" below for exactly what is encrypted, how to generate the key, and
what happens if you lose it.

The key is held the same way `git_token` is: in the add-on's options,
never written into the repository, never logged, and never passed to
`sops` on a command line. Only the derived public recipient (`age1...`)
is ever printed or committed. If the key is malformed, or the `sops`
binary is missing, the add-on fails to start rather than quietly
syncing without encryption.

Because add-on options are readable over the Supervisor API (see
`git_token` above), this is the option most worth writing as
`secret://<name>`, with the real `AGE-SECRET-KEY-1...` line under that
key in the live `secrets.yaml`. That file is plaintext on the machine -
encryption only applies to the copy pushed to git - so the key resolves
from it without any circularity, and the API then only ever sees the
pointer.

### `auto_update_addons`

Empty by default, which means off: the agent never installs an update
for another add-on unless that add-on's slug is listed here. List one
and the agent installs its updates for you as Supervisor reports them.

Entries are slugs, not display names - the last path segment of the
add-on's page URL in Home Assistant. For ESPHome, a page at
`/hassio/addon/a0d7b954_esphome/info` means the slug is
`a0d7b954_esphome`; core add-ons look like `core_samba`.

With `dry_run` on the check is report-only: the web UI and the log say
which listed add-ons have an update waiting, and nothing is installed.
With `dry_run` off the updates are installed, each one preceded by a
partial backup of just that add-on, so a bad release can be restored
from Settings > System > Backups. Restored, not rolled back: the
Rollback button does not undo an add-on update - see "Add-on
auto-update" below for the whole behavior.

The agent will not update itself, whatever the list says - its own slug
is refused, with the refusal reported rather than silently ignored.
Updating the add-on that is mid-update is not something Supervisor can
do safely, so that one stays a manual click.

The check does not run on the `interval_minutes` sync tick. It runs on
its own cadence, set by `auto_update_interval_minutes` below, plus once
shortly after the add-on starts, plus whenever you press **Check for
updates** on its card. That button runs the same check the timer runs,
so with `dry_run` off it installs what it finds.

### `auto_update_interval_minutes`

`360` by default - six hours. How often the check above asks Supervisor
whether any listed add-on is behind, from 15 minutes to 10080 (7 days).

This is not `interval_minutes`, and changing it never affects how often
your config is reconciled. Nothing here touches the repository.

Six hours is the default because it is paced to Supervisor rather than
to this agent - see "When it runs" below for why lowering it does not
learn about a release any sooner. The 15-minute floor is there for the
same reason, and because with `dry_run` off each check can install
updates and restart add-ons.

The dashboard also uses this value to decide when the add-on card's
results have gone stale, so a longer interval widens that threshold
rather than marking every row stale.

Read once at startup, like `interval_minutes`: saving it in the
Configuration tab restarts the add-on, which is what applies it.

### `track_addon_versions`

`false` by default. Turn it on and the agent records which add-ons are
installed, and what version each one is on, in
`gitops/addon-versions.yaml` on the tracked branch - rewriting it
whenever what it would write there stops matching what is committed. See
"Recording add-on versions" below.

It records what it observes, whoever changed it: a manual update from
the Home Assistant UI, Supervisor's own auto-update, `auto_update_addons`
above, a restored backup. The two options are independent and neither
needs the other.

`git_token` needs push rights, the way it does for an import. Nothing
else about the agent changes: the file is never read back, never applied,
and never copied into `/homeassistant`.

### `capture_live_changes`

`false` by default. Turn it on and the file layer becomes two-way: an
edit you make here, in the Home Assistant UI or over SSH, is committed
back to the tracked branch instead of being overwritten by the next
apply. See "Capturing live changes" below for the whole mechanism, the
conflict rules and the three things it does not cover.

`git_token` needs push rights on the branch in `branch`. A protected
branch makes the push fail every cycle - the agent reports it and holds
those files back from the apply rather than overwriting them, so nothing
is lost, but nothing syncs either until the push can land.

`dry_run` does not gate it, the line `allow_import`, `commit_back` and
`track_addon_versions` already draw: that option governs writes to Home
Assistant, and this writes to the repository. With `dry_run` on it is
arguably the safest mode the agent has - live edits flow into git and
nothing ever flows back.

**It needs a merge base to work, and one run of Import or one apply is
what creates one.** Every decision here is made against the commit the
agent last knew the repository and `/homeassistant` to agree on, so until
one of those two has happened there is nothing to compare against and the
agent stays one-way. That matters most under `dry_run`, where no apply
ever runs: turn capture on there without importing first and it will sit
at "in sync" forever without capturing anything. Run **Preview Import**,
then **Import**, and the next cycle starts capturing.

It supersedes the automatic half of `commit_back`, which would otherwise
push a throwaway branch proposing changes this has already merged. The
manual **Commit Back** button is unaffected.

### `reconcile`

Which categories of config the agent manages.

- `yaml_files` (default `true`): sync plain YAML config files.
- `registries` (default `false`): sync floors, areas, labels, helper
  entities, and entity customizations declared under `gitops/` in the
  repository. See "Registry manifests" below.
- `dashboards` (default `false`): sync Lovelace dashboards (metadata
  and view config) declared under `gitops/` in the repository. See
  "Dashboard manifest" below. Independent of `registries` - it can be
  turned on without it, and vice versa.
- `addon_options` (default `false`): sync other installed add-ons'
  options declared under `gitops/` in the repository. See "Add-on
  options manifest" below. Independent of `registries` and
  `dashboards` - it can be turned on without either.
- `integrations` (default `false`): sync config-entry integrations
  (Settings > Devices & services > Add integration) declared under
  `gitops/` in the repository. See "Integration manifest" below.
  Independent of `registries`, `dashboards` and `addon_options` - it can
  be turned on without any of them.
- `subentries` (default `false`): sync the child configurations an
  integration hangs off one of its config entries (Settings > Devices &
  services > *integration* > Add ...) declared under `gitops/` in the
  repository. See "Subentry manifest" below. Independent of every toggle
  above, `integrations` included: the parent entry can be one you set up
  by hand.
- `hacs` (default `false`): install the HACS-distributed custom
  integrations declared under `gitops/` in the repository. See "HACS
  manifest" below. Independent of every toggle above; requires HACS itself
  to be installed on the box. It only ever installs - nothing is
  uninstalled when you remove an entry.

## Registry manifests (`gitops/`)

When `reconcile.registries` is enabled, the agent also reads up to
three files from a `gitops/` directory at the root of your config
repository: `gitops/registries.yaml` (floors, areas, labels),
`gitops/helpers.yaml` (input_boolean and friends), and
`gitops/entities.yaml` (customizations of entities that already
exist). None of these files, nor the `gitops/` directory itself, is
required - if they are missing, the corresponding layer is simply
inactive that cycle.

`gitops/` is never copied into `/homeassistant`: it is the agent's own
directory - manifests it reads, plus the one file it writes for itself
(`gitops/addon-versions.yaml`, see "Recording add-on versions") - not
config the file layer syncs. This applies only to `gitops/` at the
repository root - a directory of the same name nested elsewhere in the
repo (say, bundled inside a custom component) is ordinary config and
syncs normally.

### `gitops/registries.yaml`

```yaml
floors:
  - id: ground          # manifest key, stable, required, [a-z0-9_]+
    name: Ground floor  # required
    level: 0            # optional
    icon: mdi:home       # optional
    aliases: []          # optional
areas:
  - id: living_room
    name: Living room
    floor: ground        # manifest floor id, optional
    labels: [gitops]     # manifest label ids, optional
    icon: mdi:sofa        # optional
    aliases: []           # optional
labels:
  - id: gitops
    name: GitOps
    color: indigo        # optional
    icon: mdi:source-branch  # optional
    description: ""      # optional
```

### `gitops/helpers.yaml`

Top-level keys are helper domains: `input_boolean`, `input_number`,
`input_select`, `input_text`, `input_datetime`, `counter`, `timer`.

```yaml
input_boolean:
  - id: demo_flag       # manifest key
    name: Demo flag     # required; Home Assistant derives the entity's
                         # object id from this
    icon: mdi:flag
input_number:
  - id: demo_level
    name: Demo level
    min: 0
    max: 100
    step: 5
```

Any other field you add to an item is passed straight through to Home
Assistant, so every option a domain's storage API accepts is
available, not just what's shown above.

A `timer` helper's `duration` may be written as `"H:MM:SS"`, the
shorter `"H:MM"` (hours and minutes), a plain number of seconds, or a
mapping like `{minutes: 5}` - anything Home Assistant's own duration
parser accepts. Home Assistant always echoes a duration back as
`"H:MM:SS"` with no leading zero on the hour, and this agent compares
by actual value (truncated to whole seconds, matching Home Assistant),
so switching between spellings is never reported as drift.

An `input_select` helper's `options` order is significant - Home
Assistant preserves the order it was declared in, and that is the
order shown in the dropdown, so reordering `options` in the manifest
is a real change and will be applied. An option written as an integer
or a boolean is compared by the string Home Assistant would store it
as, so `options: [1, 2]` matches a live `["1", "2"]` rather than
drifting forever. A float option is not supported and will be applied
on every run - write it as a string (`options: ["1.5"]`) instead.

### `gitops/entities.yaml`

```yaml
entities:
  - entity_id: light.living_room_ceiling  # the key; required
    name: Ceiling Light   # optional, as are all fields below
    icon: mdi:ceiling-light
    area: living_room     # a gitops/registries.yaml area id, or a
                           # live area_id directly
    labels: [managed_by_gitops]  # same resolution rule as area
    disabled: false        # false -> enabled; true -> disabled by you
    hidden: false           # false -> shown; true -> hidden by you
```

Unlike `registries.yaml` and `helpers.yaml`, this file only accepts the
six fields shown above - not every option Home Assistant's entity
registry has, on purpose (see "Ownership" below for why). Renaming an
entity (`new_entity_id`) is not supported; declaring it is a validation
error.

### Ownership

**`gitops/entities.yaml` follows a different, narrower model than the
rest of this section: it only ever customizes entities that already
exist, never creates or deletes one.** Entities come from your
integrations - lights, sensors, switches and so on - not from this
agent, and it never will create or delete one on your behalf.

- A declared `entity_id` that doesn't exist in Home Assistant is
  reported as an error ("entity not found") and left alone - never
  created.
- The first time an entity comes under management, the agent records
  the live value of each field you declared for it, before changing
  anything. Only ever those declared fields are compared or touched on
  every later reconcile - anything about the entity you didn't declare
  is never read, compared, or sent.
- Remove an entity from the manifest (or leave its `entity_id` with no
  other fields declared) and the agent restores every field it ever
  recorded an original value for, back to what it was before this
  agent touched it, then forgets it. This is this layer's version of
  "delete": the entity itself is never removed, only put back the way
  it was. **Restore is all-or-nothing per `entity_id`, not per field:**
  removing a single field from an entity that is still otherwise
  declared neither restores that one field nor releases it from
  management - it keeps whatever value it last had, still tracked, and
  only comes back on the manifest's own next declared value for it (or
  gets restored, along with every other recorded field, once the whole
  `entity_id` is removed from the manifest).
- If an entity is already disabled or hidden by something other than a
  person - an integration, a config entry, a device - the agent
  refuses to touch it at all (for either a customization or a
  restore) and reports why, rather than fighting whatever put it in
  that state.

### Ownership (floors, areas, labels, helpers)

The agent only ever touches objects you have declared in `gitops/`:

- An object it previously created or adopted stays in sync with the
  manifest on every reconcile: edit its name, icon, or any other
  field in the manifest and that edit gets applied.
- Remove an item from the manifest and, if the agent manages it, the
  live object is deleted. This also applies to removing every item at
  once, deleting a manifest file entirely, or deleting the whole
  `gitops/` directory: anything still listed under this agent's
  ownership gets cleaned up on the next reconcile, exactly as if each
  item had been removed one at a time. Objects the agent has never
  created or adopted are never edited or deleted, no matter what the
  manifest says.
- Point the manifest at a live object you already created by hand
  (same `name`, not yet managed) and the agent adopts it by that exact
  name match instead of creating a duplicate. If more than one
  existing object shares that name, adoption is ambiguous: the agent
  skips that item and reports the conflict rather than guessing.
- If a managed object is deleted out-of-band (say, by hand in the Home
  Assistant UI), the agent recreates it from the manifest on the next
  reconcile.

Secrets and anything not declared in `gitops/` are never touched by
this layer, exactly as with the file sync layer.

## Dashboard manifest (`gitops/`)

When `reconcile.dashboards` is enabled, the agent reads
`gitops/dashboards.yaml` from the config repository, independently of
`reconcile.registries` - you can turn on either without the other.
Missing entirely, this file means the layer is inactive that cycle -
but only for as long as the agent manages nothing. Once it manages a
dashboard, a missing or emptied `gitops/dashboards.yaml` reads as
"delete every dashboard I manage", and the next apply deletes them,
view configs and all. Deleting the file is not how you switch this
layer off: set `reconcile.dashboards: false` instead, which leaves
everything it manages exactly where it is.

```yaml
dashboards:
  - id: gitops-home            # required; becomes/matches the dashboard's url_path
    title: GitOps Home         # required
    icon: mdi:view-dashboard   # optional
    config: gitops/dashboards/home.yaml  # repo-relative Lovelace view config; required
    show_in_sidebar: true      # optional, default true
```

`id` is the dashboard's `url_path`, the part of its URL after the host,
so it has to match exactly to adopt a dashboard that already exists.
Home Assistant rejects a new dashboard whose url_path has no hyphen
unless the caller explicitly asks it to allow one, which its own
dashboard dialog does not do - so a dashboard you made by hand has a
hyphen in its url_path, and adopting it means declaring that exact
hyphenated id, for example `id: my-dashboard`. Take it from the URL,
not from the id column: for `my-dashboard` Home Assistant shows an
internal id of `my_dashboard`, and declaring that underscored form
builds a second dashboard rather than adopting the first. For a
dashboard the agent creates itself either spelling works, since it does
ask for the exception.

`config` points at a second, repo-relative YAML file: the dashboard's
actual Lovelace view configuration - views, cards, everything the
Lovelace UI editor would normally save for you. Edit that file and push
it, and the agent picks up the change on its next reconcile exactly
like a metadata edit.

Any path in the repository works for `config`, but keeping it under
`gitops/` (as above) is tidier: `gitops/` is the one directory the file
sync layer skips, so a view config anywhere else also gets copied into
your config directory, where Home Assistant never reads it. Harmless,
just a stray file.

The ids `default` and `lovelace` can never be declared: the built-in,
unnamed default dashboard has no id and cannot be managed this way, and
`lovelace` is the url_path Home Assistant itself may already be using
for it - trying to declare either is a validation error at manifest
load time, not something that fails later at apply time.

### Ownership (dashboards)

Dashboards follow the exact same create/adopt/update/delete-only-managed
model as floors, areas, and labels - see "Ownership (floors, areas,
labels, helpers)" above - matched by `id`/url_path instead of by name.
Since Home Assistant itself guarantees a dashboard's url_path is
unique, adopting one by matching url_path is never ambiguous the way
adopting a floor/area/label by name can be.

Metadata (title, icon, sidebar visibility) and view content are
compared and applied independently: editing only the title in the
manifest never touches the saved view config, and editing only the
view config file never touches the title. A dashboard's content is
destroyed along with it when the agent deletes a dashboard it no
longer manages - there is no separate content-only delete.

## Add-on options manifest (`gitops/`)

When `reconcile.addon_options` is enabled, the agent reads
`gitops/addons.yaml` from the config repository, independently of
`reconcile.registries` and `reconcile.dashboards` - you can turn any
of the three on without the others. Missing entirely, this file means
the layer is inactive that cycle - but only for as long as the agent
manages nothing. Once it has changed an add-on's options, a missing or
emptied `gitops/addons.yaml` reads as "restore everything I changed",
and the next apply writes those recorded originals back and forgets the
add-on (see "Ownership (add-ons)" below). To switch this layer off
without that happening, set `reconcile.addon_options: false` instead of
deleting the file.

```yaml
addons:
  - slug: core_configurator    # the other add-on's Supervisor slug; required
    options:                   # required, non-empty; ONLY these keys are managed
      dirsfirst: true
    restart_on_change: true    # optional, default true
```

Unlike `registries.yaml`/`dashboards.yaml`, `options` is not a fixed
field list: it is whatever keys that add-on's own configuration
accepts, since every add-on defines its own options. Only the keys
you actually declare here are ever read, compared, or written -
everything else about the add-on's configuration is left exactly as
it is, including options set by hand through its own Supervisor
config page.

An option value written as `secret://<name>` is read out of the live
`/homeassistant/secrets.yaml` instead of out of the repository - see
"Referencing secrets" under the integration manifest above, which
describes the one mechanism all three layers share.

### Ownership (add-ons)

**`gitops/addons.yaml` follows the same narrower, update-only model as
`gitops/entities.yaml`: it only ever customizes an add-on that is
already installed, never installs or uninstalls one.**

- A declared `slug` that is not installed - unknown to Supervisor
  entirely, or known but never installed - is reported as an error
  ("add-on not installed") and left alone.
- **This agent can never manage its own options.** The add-on it is
  running as is resolved once at startup and always refused, with its
  own error explaining why - there is no way to declare it in the
  manifest and have that take effect.
- The first time an add-on's options come under management, the agent
  records the live value of each key you declared for it, before
  changing anything. Only ever those declared keys are compared or
  touched on every later reconcile. A key that had no value at all is
  recorded as having had none, which it keeps distinct from a key that
  was set to null.
- Remove an add-on from the manifest and the agent restores every key
  it ever recorded an original value for, back to what it was before
  this agent touched it, then forgets it - the same "restore, never
  delete" model `gitops/entities.yaml` uses. A key that had no value
  before is restored by taking it back out of the add-on's options, not
  by setting it to null: Supervisor reads an option that is present
  with no value as a required option nobody supplied and rejects the
  write, even for a key the add-on's own configuration marks optional.
- Writing options is always read-merge-write: the agent fetches the
  add-on's current options fresh, overlays only your declared keys,
  and writes the whole object back, so any option you did not declare
  - including ones set some other way - is never dropped. A cycle that
  finds nothing to change never writes anything at all.
- **Once an apply does change something, every currently-effective
  value for that add-on gets pinned, not just the key you declared.**
  Supervisor's API has no reliable way to tell an add-on's own
  built-in default apart from a real persisted override for an
  add-on that is already installed - there is no endpoint that
  returns just the defaults for one, so the agent cannot separate
  "this value is only a default" from "this value was actually set"
  before writing back. The practical effect: after the first apply
  that touches an add-on, every option it had at that moment -
  declared or not - becomes a real persisted value from then on. If
  that add-on later ships a new version with a different built-in
  default for a key you never declared, that new default will not
  take effect on its own; you would need to declare the key yourself
  (or change it by hand) to move off the value pinned at that first
  apply.
- If a change actually altered a value and `restart_on_change` is
  true (the default), the agent restarts the add-on afterward and
  waits for it to report running again before considering the change
  applied. Set `restart_on_change: false` for an add-on whose options
  take effect without a restart, or where you would rather restart it
  yourself on your own schedule.

## Integration manifest (`gitops/`)

When `reconcile.integrations` is enabled, the agent reads
`gitops/integrations.yaml` from the config repository, independently of
`reconcile.registries`, `reconcile.dashboards` and `reconcile.addon_options`
- you can turn any of the four on without the others. Missing entirely,
this file means the layer is inactive that cycle - but only for as long
as the agent manages nothing.

**Once it manages a config entry, a missing or emptied
`gitops/integrations.yaml` reads as "delete every integration I
manage", and the next apply deletes those entries - along with every
device and entity they created, permanently, as described under
"Ownership (integrations)" below. Never delete this file to switch the
layer off. Set `reconcile.integrations: false`, which leaves what it
manages alone.**

```yaml
integrations:
  - id: workday_main           # manifest key; required
    domain: workday            # the integration's domain; required
    title: Workday             # required; adopt matches by domain + exact title
    data:                      # optional; flow input per step id
      user: {name: Workday, country: PL}
```

`data` maps each step of the integration's setup flow to that step's
field values - exactly what you would type into the "Add integration"
dialog. Most built-in integrations with a plain form-based setup -
`workday`, `local_ip`, `moon`, `time_date` and similar - need only one
step, sometimes none.

Leave `data` out entirely when every step of the flow asks for nothing:
`moon` and `local_ip` each have a single step with no fields at all, and
both are set up by declaring only `id`, `domain` and `title`. A step
that asks for nothing needs nothing declared for it. Anything you do
declare for such a step is still submitted as it stands, and an
integration is free to reject a field it never asked for.

**To find out what a step does want, declare the integration without it
and read the error.** The agent names the fields, reading them off the
schema Home Assistant sends along with the step:

```
create integration:time_now failed: domain time_date: flow step 'user'
has no declared data in the manifest (add a data.user mapping). Step
'user' accepts: display_options (required, select: time, date,
date_time, date_time_utc, date_time_iso, time_date, time_utc)
```

That is enough to write `data: {user: {display_options: date_time}}`
without opening the dialog at all. Every field is listed with whether it
is required or optional and what kind of input it takes; a dropdown's
accepted values are spelled out, and a very long list of them is cut
short rather than filling the events feed. A field that takes several
values at once is marked `(multiple)` and wants a YAML list; one without
that mark, like `display_options` above, wants the bare value and
rejects a single-item list.

`title` is written onto the entry once its setup flow finishes. The flow
names the entry itself - `time_date`'s comes out "Time & Date time",
`local_ip`'s "local_ip" - and the agent then renames it to the `title`
you declared. That rename is what keeps `title` meaningful: adoption
matches an existing entry by domain plus its exact live title, so an
entry left under the flow's own name could never be adopted back by the
manifest item that created it, and a reconcile that had lost track of it
(a reinstall, a restored backup) would create a duplicate beside it.

If the rename fails, the integration is still created and still managed:
the failure is reported against that item for that apply, and the entry
keeps the title the flow gave it. **That report is the only one you
get.** Later reconciles will not repeat it, because an integration
already under management is compared by its declared `data` alone -
nothing plans a rename for an entry that is already managed. Rename it
yourself in the Home Assistant UI to match the manifest.

**The same holds for anything created before this add-on wrote titles
at all.** On an install upgraded from an earlier version, entries the
agent created still carry the title their flow chose, and nothing will
retitle them. They keep working, but the duplicate hazard above stays
with them until you rename them by hand to match the manifest - or
remove them from the manifest, let that apply delete them, and declare
them again.

**Anything you put in `data` - including credentials or API keys some
integrations require at setup - ends up stored in more places than just
the repository clone.** Applying a create/adopt/delete for that
integration snapshots it into that apply's stash directory under
`/data/backup/<timestamp>/` (see "Backups before every apply" below),
and the last 5 of those are always kept on disk. Because
`/data` is this add-on's persistent storage, it is included in any
Supervisor backup (full or partial) that includes this add-on - which
means a backup a user uploads or exports off-device carries that data
along with it. Treat anything you put in a flow's `data` the same way
you would treat a secret checked into the repository.

### Referencing secrets

Or do not put it there at all. Any declared value of the exact form
`secret://<name>` is replaced, when the plan is computed, by whatever
`/homeassistant/secrets.yaml` holds under that name on the box:

```yaml
integrations:
  - id: anker
    domain: anker
    title: Anker Solix
    data:
      user:
        username: solar@example.com
        password: secret://anker_password
```

```yaml
# /homeassistant/secrets.yaml, on the box - not in the repository
anker_password: the-real-password
```

The name is the same kind of key Home Assistant's own `!secret` tag reads
out of that file, and it is the same file - so a value you already
reference from `configuration.yaml` can be referenced from a manifest
without copying it anywhere. Nothing after `secret://` is trimmed or
guessed: `secret://` on its own, or a name with a space in it, is
reported as a malformed reference rather than quietly looked up as
something else.

**The same syntax works in `gitops/subentries.yaml` (any declared `data`
value) and in `gitops/addons.yaml` (any declared option value).** Those
three manifests are the whole list. `registries.yaml`, `helpers.yaml`,
`entities.yaml` and `dashboards.yaml` do **not** resolve references -
none of them declares a credential, and a `secret://` written in one is
passed through to Home Assistant as the literal string it looks like.

**The value must be a plain scalar.** A string, a number or a boolean,
and it arrives exactly as the file spells it - `007` stays `007`, a long
token keeps every digit. A name whose value is a list, a mapping, or
carries a Home Assistant loader tag (`!include`, `!env_var`, and friends
are all legal in `secrets.yaml`) is refused by name rather than turned
into whatever text happens to sit after the tag.

**What the repository holds is the reference.** The value never enters
the repository, so the repository copy of `secrets.yaml` can stay
encrypted (see "Secret encryption (SOPS and age)" below) and the manifest
that uses it needs no encryption at all. It does not enter `state.json`
either: what is stored under `integration_data` for a rollback replay is
the reference as written, and it is resolved again - against the file as
it stands at that moment - if that rollback ever runs. The per-apply
rollback stashes under `/data/backup/<timestamp>/` hold the reference
too. The plan lines on the dashboard, the activity feed, the log and
`/status.json` name the reference and never the value; an add-on options
line that would otherwise print the value shows
`password: (hidden) -> 'secret://mqtt_password'` instead.

**This applies from the moment an item is created with the reference in
place.** Switching an integration that is ALREADY managed from a literal
value to a `secret://` reference naming the same value changes nothing
the agent can see - the fingerprint it compares is taken after
resolution, so it is identical, nothing is planned, and the literal
already written into `integration_data` stays there. To get it out:
remove the item from `gitops/integrations.yaml`, let that apply delete
it, then declare it again with the reference. A subentry has nothing to
purge (it stores no declared data at all), and an add-on option's
`addon_originals` entry holds the value the add-on had *before* this
agent touched it, never the manifest's.

**A rollback does not put a referenced add-on option back.** Every other
key an apply touched is restored; a key whose value came from a
reference is left where the apply put it, because the value it held
before is exactly what the stash is not allowed to keep. The message
after the rollback says what was restored; if you need that key back,
change `secrets.yaml` (or the manifest) and let the next reconcile
converge it.

**Rotating a secret is a change.** These layers compare a fingerprint of
the data they last applied, and that fingerprint is taken after
resolution - so editing `secrets.yaml` alone, with no manifest change at
all, is drift. If the file itself is one the agent syncs (an encrypted
`secrets.yaml` in the repository), that takes two cycles rather than one:
the first writes the new file into the live config, and the manifest
layers - which read the live file, not the repository - see the new value
on the cycle after. A subentry converges onto the new value through an
ordinary reconfigure. An integration cannot (Home Assistant has no way
to edit a config entry's data - see "Ownership (integrations)" below), so
it is reported the same way any other `data` edit is: remove it from the
manifest, let that apply, and declare it again. An add-on option is
simply written again.

**A name that is not in the file blocks that item and nothing else.** A
missing `secrets.yaml`, a name it does not carry, or a name whose value
is a list or a mapping rather than a plain scalar, is reported against
that item alone, naming the key - "secrets.yaml has no key
'anker_password'" - and never quoting the value. The rest of the
manifest, and every other layer, reconciles as usual, and nothing is
attempted for the blocked item. This one is decided while the plan is
computed rather than while it is applied, so it is not a remembered
failure and needs no **Retry**: add the key to `secrets.yaml` and the
next check - the interval, or the **Check now** button - plans the item
normally.

One gap worth knowing, and it is one-shot: an add-on option managed
through a reference and then *removed* from `gitops/addons.yaml` plans a
restore, and by then there is no manifest entry left to say the value
came from a reference. That restore's plan line renders the add-on's
current live value - the resolved secret - and its rollback-stash entry
records it. Un-manage such an add-on with `reconcile.addon_options:
false` instead if that matters; that leaves the options where they are
and plans nothing.

### What works and what does not (bounded v1)

This layer only drives plain, form-based setup flows. It cannot handle:

- **OAuth or other external-auth flows** (anything that redirects you to
  a browser, e.g. Google/Nest/Spotify-style integrations).
- **Discovery-driven flows** (integrations that only offer to set
  themselves up once Home Assistant discovers a device on the network).
- **Options flows** (reconfiguring an integration that is already set
  up - this layer only ever creates a fresh one or deletes an existing
  one, see "Ownership" below).
- **Reauthentication flows.**

If a declared integration's setup flow hits any step this layer cannot
answer - one of the kinds above, or a plain form step whose data you
didn't declare, or one Home Assistant itself rejected as invalid - the
agent aborts that flow (Home Assistant is left exactly as if you had
closed the "Add integration" dialog partway through: nothing is created)
and reports the problem as an error for that item, quoting the step and
why. It never leaves a half-finished setup behind.

### Ownership (integrations)

- Point the manifest at an integration you already set up by hand (same
  `domain`, same exact `title`) and the agent adopts it instead of
  creating a duplicate - no setup flow runs, it just starts tracking the
  existing one. If more than one existing entry shares that domain and
  title, adoption is ambiguous: the agent skips that item and reports the
  conflict rather than guessing.
- No match at all -> the agent drives the setup flow itself, using the
  data you declared, and starts tracking the entry it creates.
- **There is no "update" for a config entry's data once it exists** -
  Home Assistant has no API for editing one directly, only creating a new
  one or removing it. So once an integration is under management, editing
  its `data` in the manifest does **not** change anything live - the
  agent instead reports an error telling you to remove it from the
  manifest (let that apply run, which deletes it) and re-declare it
  (which creates it fresh with the new data). This is different from
  every other `gitops/` manifest in this add-on, where an edit just takes
  effect on the next reconcile.
- Remove an integration from the manifest and, if the agent manages it,
  the entry is deleted - exactly like removing a floor, area, label,
  helper, or dashboard. Integrations it has never created or adopted are
  never touched. **This is destructive: deleting a config entry deletes
  every device and entity that integration created, permanently, the
  same as removing it by hand in the Home Assistant UI.** Automations,
  scripts, dashboards, and anything else in `gitops/entities.yaml` that
  reference those entities will break. Re-declaring the integration runs
  its setup flow fresh (see the "No match at all" bullet above) and
  produces a new entry with new entity IDs, not the ones that were
  deleted, so those references need updating by hand regardless.
- If a managed entry is deleted out-of-band (say, by hand in the Home
  Assistant UI), the agent recreates it from the manifest on the next
  reconcile - unless another entry with the same domain and exact title
  has since appeared, in which case it adopts that one instead.
- **Rolling back a deleted integration re-creates it, but with a new
  identity.** The agent replays the same setup flow it would use to
  create the integration fresh - it cannot restore the exact same entry,
  because Home Assistant does not let anything read a config entry's
  setup data back out once the flow that created it has finished. The new
  entry works the same way, but any of its entities will have new entity
  IDs, and anything you customized about them (via `gitops/entities.yaml`
  or by hand) will need to be redone.
- **Each declared integration applies completely independently of every
  other one.** Integrations never reference each other the way an area
  references a floor, or an entity references an area or label, so one
  integration's failure never undoes another integration that already
  applied successfully in the same reconcile - unlike registries,
  entities, dashboards, and add-on options, none of which this applies
  to. If your manifest declares five integrations and the fourth one's
  flow fails, the first three stay created/adopted and only the fourth
  is reported as failed; the fifth still gets its own attempt too.
- **A create that fails is remembered, and is not retried on its own.**
  If a declared integration's setup flow fails - most commonly a step
  whose `data` you have not declared - the agent will not keep re-driving
  that same failing flow on every reconcile interval forever. It records
  a fingerprint of the exact declared `data` that failed, and reports a
  per-item error explaining that a previous attempt failed (with a short
  reason) until that fingerprint changes - editing the item's `data` (the
  same thing rule 2 above already fingerprints to detect an edit after
  creation) is what unblocks a retry; changing only `domain` or `title`
  with the same `data` does not. This also means: after fixing a typo in
  `data`, you must actually change the manifest text, or clear the record
  by hand - re-running a reconcile against byte-identical data that
  already failed keeps reporting the same remembered failure rather than
  trying again. The dashboard's **Recorded failures** card lists every
  remembered failure with a **Retry** button that clears that one record,
  which is what you want when the cause was outside the repository (a
  password changed at the vendor, a device that was not reachable yet)
  and the manifest is already correct.

## Subentry manifest (`gitops/`)

When `reconcile.subentries` is enabled, the agent reads
`gitops/subentries.yaml` from the config repository, independently of
every other `reconcile` toggle - `integrations` included, so the parent
integration can be one you set up by hand. Missing entirely, this file
means the layer is inactive that cycle.

A subentry is the child configuration a modern integration hangs off one
of its config entries: a calendar under Google Calendar, a room under
generic thermostat, a widget under PushWard. In the UI you add one from
Settings > Devices & services > *integration* > Add *thing*.

```yaml
subentries:
  - id: widget_house_stats     # manifest key; required, [a-z0-9_]+, unique
    domain: pushward           # parent entry's integration domain; required
    entry_title: PushWard      # optional; picks one when the domain has
                               # more than one config entry. Renaming that
                               # entry in Home Assistant later is safe: a
                               # subentry already under management is found
                               # by its own id, not by this title. Changing
                               # `domain` is not - that points the item at a
                               # different integration, so remove the item,
                               # let it un-manage, and declare a new one.
    subentry_type: tracked_widget   # which "Add ..." flow to drive; required
    match:                     # how to recognize an existing subentry
      unique_id: house-stats   # at least one of unique_id / title
    data:                      # optional; flow input per step id
      user:
        entity_id: sensor.house_power
        widget_template: stat_list
        slug: house-stats
      details:
        widget_name: House stats
        stat_rows:
          - {label: Power, entity_id: sensor.house_power, unit: W}
          - {label: Water, entity_id: sensor.water_today, unit: L}
```

`match` is how the agent recognizes a subentry that already exists, so it
adopts it instead of creating a second one. `unique_id` is tried first
because it is stable - PushWard uses the widget's own slug - and `title`
only as a fallback, since a title is user-editable. Matching is always
scoped to the resolved parent entry and the declared `subentry_type`, so
two subentries of different types sharing a title never collide.

`data` maps each step of that flow to that step's field values, exactly
like `gitops/integrations.yaml`. The example above is a two-step flow:
step `user` picks the entity, the template and the slug, step `details`
fills in what that template needs - here the `stat_rows` list, one row
per line of the widget. A `data` value written as `secret://<name>` is
read out of the live `/homeassistant/secrets.yaml` - see "Referencing
secrets" under the integration manifest above. Rotating such a secret is
drift here in the most useful way there is: the next reconcile plans a
reconfigure that submits the new value.

### Undeclared fields keep their live values

A reconfigure form arrives pre-filled with the subentry's **current**
values, and the agent submits the form's own defaults for every field
the manifest does not declare. So a partial `data` block edits only the
fields it names and leaves the rest of the live subentry alone. Drop
`widget_name` from the example above and the widget keeps whatever name
it has now; it does not revert to empty.

This is the opposite of how `gitops/entities.yaml` and
`gitops/addons.yaml` treat an undeclared field (both restore it when you
stop declaring it), and it is not a choice this add-on gets to make: the
data behind a subentry cannot be read back out, so there is no "what it
was before this manifest touched it" to restore.

### One manifest, two names for the first step

Home Assistant calls the first form of a **create** flow `user` and the
first form of a **reconfigure** flow `reconfigure`, even though the two
ask for the same fields. Declare either one and the agent answers
whichever the live flow actually presents - there is no need to write the
same block twice. If you do declare both, the one whose name matches the
live step wins; the alias is only ever a fallback.

### To learn a step's fields, declare nothing for it and read the error

The same discovery trick the integration manifest documents works here.
Declare the subentry with no `data` (or without the step you do not know
yet), let one reconcile run, and the error names the fields, reading them
off the schema Home Assistant sends with the step - each one marked
required or optional, with a dropdown's accepted values spelled out. Then
write the `data` block from that.

### What counts as drift (and what does not)

The data behind a live subentry is unreadable: Home Assistant lists a
parent entry's subentries with only `subentry_id`, `subentry_type`,
`title` and `unique_id`, never the data a flow submitted. So this layer
cannot diff declared against live the way the registry, entity and
dashboard layers do. Instead it stores a fingerprint of the data it last
applied and treats a change to **that** as the signal to converge, the
same model `gitops/integrations.yaml` uses.

**The blind spot that follows: an edit made in the UI to a field this
manifest declares is invisible here.** The fingerprint still matches the
still-unchanged manifest, so nothing is reported and nothing converges it
back. The declared value only reasserts itself the next time the manifest
text itself changes. If somebody renames a stat row on the box, the
repository will not notice and will not fix it.

Unlike integrations, though, a subentry **can** be updated in place: it
supports a reconfigure flow, so editing `data` in the manifest applies
that edit live on the next reconcile. There is no delete-and-recreate
dance.

### Ownership (subentries)

- Point the manifest at a subentry you already added by hand (same
  parent, same `subentry_type`, matching `unique_id` or `title`) and the
  agent adopts it. Adoption immediately runs a reconfigure with the
  declared data, rather than trusting a subentry it has never looked
  inside - so an adopted subentry converges on the first apply.
- No match at all -> the agent drives the create flow and starts tracking
  the subentry it makes.
- **This layer never deletes anything.** Remove an item from the
  manifest and the agent stops managing it: the state key is dropped and
  the live subentry is left exactly as it is, still working. That is the
  whole effect. Emptying or deleting `gitops/subentries.yaml` is
  therefore safe here, unlike `gitops/integrations.yaml`, where it means
  "delete every integration I manage". To remove a subentry, delete it in
  the Home Assistant UI.
- **There is no rollback.** The Roll Back button does not undo a
  subentry apply, because the previous data cannot be read and so cannot
  be restored. An unwanted reconfigure is undone by hand, or by another
  manifest change. Everything else in the same apply still rolls back
  normally; this layer simply has nothing stashed.
- **Each declared subentry applies independently of every other one**, as
  integrations do: if the fourth of five fails, the first three stay
  applied and the fifth still gets its own attempt.
- **A failed flow is remembered and is not retried on its own.** The
  agent records the fingerprint of the data that failed and reports that
  failure until the fingerprint changes, so a broken declaration does not
  re-drive the same failing flow every interval. Editing the item's
  `data` is what unblocks a retry, or the **Retry** button on the
  dashboard's **Recorded failures** card, which clears that one record.

### PushWard: a created widget is not visible yet

Creating a `tracked_widget` subentry configures the widget on the Home
Assistant side. It does **not** put it on anybody's phone: a PushWard
home-screen widget shows the slug that was picked for it in the phone's
own widget settings. A brand new slug has to be selected there once, by
hand, before it renders anything. After that the manifest owns its
contents, and further edits arrive without touching the phone.

## HACS manifest (`gitops/`)

When `reconcile.hacs` is enabled, the agent reads `gitops/hacs.yaml` from
the config repository, independently of every other `reconcile` toggle,
and downloads the custom integrations it declares through
[HACS](https://hacs.xyz). Missing entirely, this file means the layer is
inactive that cycle. Unlike `gitops/integrations.yaml`, deleting or
emptying it is safe: this layer has no uninstall path at all (see
"Ownership (HACS)" below).

**HACS itself has to be installed on the box.** The agent drives HACS's
own WebSocket commands - the same ones the HACS panel uses - so a box
without it skips this layer, says so with a standing "hacs layer: HACS is
not installed" chip on the dashboard, and changes nothing. Everything else
in the same cycle still runs. Install HACS the normal way first, or turn
`reconcile.hacs` off.

**Turning this on converts write access to your repository into the
ability to run arbitrary third-party Python inside Home Assistant.** A
HACS integration is not configuration: it is code, downloaded from GitHub
and imported into the same process that holds your Home Assistant
credentials, your network and your `secrets.yaml`. Anyone who can commit
to the tracked branch - a colleague, a compromised token, a merged pull
request nobody read closely - can add four lines to `gitops/hacs.yaml` and
have that code installed on the next cycle. With `dry_run: false` that
needs no further human action at all. Treat push access to this repository
as equivalent to shell access to the box, and:

- **Pin `version`.** An unpinned entry takes whatever the repository's tip
  offers at the moment the download runs, so the same manifest installs
  different code on different days.
- `version` accepts a release tag, a branch name or a commit SHA, and they
  are not equally strong: a tag can be moved and a branch is a moving
  target by definition, while a **commit SHA** names exactly one tree and
  is the only value that cannot change under you.
- Review what you declare the way you would review a dependency, and
  remember that a pin only fixes the FIRST install - see below.

```yaml
hacs:
  - id: anker_solix                       # manifest key; required, [a-z0-9_]+, unique
    repository: thomluther/ha-anker-solix # owner/name; a full github URL works too
    category: integration                 # required; only "integration" is supported
    version: "3.1.0"                      # optional; used at install time only
```

`repository` is the GitHub repository HACS knows the integration by. Both
`owner/name` and the full `https://github.com/owner/name` URL you copied
out of the address bar are accepted; the agent normalizes them to the same
thing. A repository HACS does not already list is added as a custom
repository first, exactly as the panel's "Custom repositories" dialog
does, and then downloaded.

`category` accepts `integration` and nothing else today. HACS also
distributes Lovelace plugins, themes, python scripts, AppDaemon apps and
templates; declaring one of those is a per-item error rather than a line
that quietly does nothing.

### `version` is used at install time only, and never reconciled

A pinned `version` is what a fresh download asks HACS for. Once the
integration is installed, this layer plans nothing for it at all - so
editing `version` later neither upgrades nor downgrades anything, and the
dashboard reports no drift for a version that no longer matches. Quote it:
YAML reads an unquoted `3.10` as the number `3.1`, so an unquoted version
is refused rather than turned into a release tag that does not exist.

Keeping installed integrations up to date is HACS's own job, through its
panel and its update entities. Doing it from here would mean this add-on
replacing live code under a running Home Assistant on a five-minute timer.

### Downloaded is not loaded

A freshly downloaded custom integration is on disk and **not loaded**:
Home Assistant imports `custom_components/` at startup. Until it restarts,
the domain does not exist, its services are not registered, and a config
entry cannot be created for it.

The dashboard says so next to the state pill: *downloaded, not loaded yet:
anker_solix - restart Home Assistant, then set it up*. Both halves are
meant literally, and the second one is the part that surprises people:
Home Assistant reports an integration as loaded once something has SET IT
UP, so a restart alone does not clear the reminder for an integration that
has no config entry yet. Add it from Settings > Devices & services, or
declare its entry in `gitops/integrations.yaml` and let the agent do it.
The chip goes away on the first check after that.

The agent never restarts Home Assistant on its own - that is your call,
and it is the same restart the HACS panel asks for after a download there.

Declaring the integration in `gitops/hacs.yaml` and its config entry in
`gitops/integrations.yaml` in one commit is the normal way to do this, and
it works, but not in one cycle: the download is applied first (the agent
orders the layers that way deliberately), and the flow that creates the
entry cannot succeed until the restart. Home Assistant answers it with
"Invalid handler specified" until then. The agent reports that per cycle
and deliberately does **not** remember it as a failure - so once you
restart, the entry is created on the next check with nothing to press.

### Ownership (HACS)

- Declare a repository that is already installed and the agent adopts it:
  one bookkeeping change on the next apply, nothing downloaded, no version
  touched. Ownership is recorded by an apply, not by looking - which is
  why an adopted item shows up once as a pending change and then never
  again.
- Declare one that is missing and the agent downloads it, then starts
  tracking it.
- **This layer never uninstalls anything.** Remove an item from the
  manifest and the agent simply stops following it: the integration stays
  installed and working, and the ownership record stays in the "Managed by
  this agent" card until you clear it. Removing a custom component takes
  its entities, its config entries and their history with it, which is too
  destructive to trigger by deleting a few manifest lines.
- **To remove one, take it out of the manifest first, then uninstall it in
  the HACS panel** - in that order. A repository that is still declared
  but no longer installed is drift, and the next cycle downloads it again;
  uninstalling first therefore just starts a reinstall loop. Removing the
  manifest entry first makes the agent stop looking at it, and the panel
  then has the last word.
- **A pinned `version` is not a lock.** It is what a fresh download asks
  for, and nothing reconciles it afterwards: if the integration is updated
  from the HACS panel, the manifest still says the old version and the
  agent reports no drift. The pin makes a REBUILD reproducible, it does
  not hold a running box at that version.
- **An `id` owns one repository for as long as it exists.** Pointing an
  existing id at a different `repository` is refused with an error naming
  both, rather than silently rebinding: the ownership record is the only
  trace that this add-on installed the first one, and a layer that never
  uninstalls would otherwise leave it on the box with nothing saying where
  it came from. Declare the new repository under a new id, or remove the
  old entry first.
- **There is no rollback.** The Roll Back button does not un-download
  anything, for the same reason. Everything else in the same apply still
  rolls back normally; this layer simply has nothing stashed.
- **Each declared repository applies independently of every other one**,
  as integrations and subentries do: if the fourth of five fails, the
  first three stay installed and the fifth still gets its own attempt.
- **A failed download is remembered and is not retried on its own.** A
  repository that does not exist, a pinned version with no such release, a
  GitHub rate limit: the agent records the failure and reports it until
  the manifest entry itself changes, so a broken declaration does not
  re-download every interval. Editing the entry unblocks a retry, as does
  the **Retry** button on the dashboard's **Recorded failures** card,
  which clears that one record - the right button when the cause was
  outside the repository and has since gone away.

## A broken manifest stops the whole cycle

Every per-item problem described in the sections above is contained to
that item: a dashboard whose view config will not parse, an entity that
does not exist, an add-on that is not installed, an integration whose
setup flow fails. Each is reported against the item it belongs to, and
everything else in that manifest still applies.

A structural problem with a `gitops/` manifest is not contained. Invalid
YAML, a top level that is not a mapping, a missing or malformed `id`, a
field the manifest does not support - any of these ends the entire
reconcile cycle where the file failed to load. Nothing else is planned
and nothing else is applied: not the other `gitops/` manifests, and not
the plain YAML file sync either, even though it has nothing to do with
the file that is broken. A file edit you pushed in the same commit is
still sitting there, unapplied, and so is everything the other manifests
declare.

This is deliberate. A manifest the agent cannot read is one where it
cannot tell "you removed this item" apart from "this file is broken",
and the safe reading of the second is to touch nothing at all. Nothing
live is changed by a cycle that ends this way.

Such a cycle also throws away the plan it was carrying, so the dashboard
shows the error with no pending list beside it. That is not "there is
nothing to do" - it is "nothing was compared". The list the last
successful cycle left behind was computed against a different commit than
the one now checked out, and the one thing it certainly does not account
for is the manifest that is now broken; leaving it up would invite
applying a plan the repository no longer matches. Apply is refused for
the same reason while this error stands: with no plan behind it, an apply
would write nothing, report success, and clear an error it never looked
at, leaving the dashboard green while a whole layer is not running. Roll
Back is unaffected and stays available - it restores the last apply's own
saved copies and needs no plan at all.

Fix the manifest the error names, push, and the next cycle plans
everything normally again - including the file edit that has been waiting.

## Drift commit-back

Enabled by the `commit_back` option (default `false`). Captures live
drift in the FILE layer only - it never touches registry, entity,
dashboard, add-on option, integration, subentry or HACS drift.

There are two ways to trigger it:

- **Automatically**, at the end of a reconcile that found pending file
  drift, but only while both `commit_back` and `dry_run` are on. If
  `dry_run` is off, a real apply already reconciles the drift live, so
  there is nothing left to capture.
- **On demand**, via the "Commit Back" button in the web UI,
  shown whenever `commit_back` is enabled and there is pending file
  drift - regardless of `dry_run`.

`capture_live_changes` supersedes the automatic trigger. Both would fire
on the same drift, one pushing a throwaway branch proposing exactly what
the other has already committed to the tracked branch. The button stays:
parking a set for review on purpose is still worth having. See "Capturing
live changes" below.

Either way, the agent creates a new branch named
`gitops/drift-<UTC timestamp>` from the tip it last fetched, writes the
CURRENT live content of every drifted file into that branch (a file
still on live gets its content copied in; a file no longer on live gets
removed from the branch), commits with the message `drift: capture live
changes from home assistant`, and pushes it - using the same
`git_username`/`git_token` credentials every other operation uses.
**It never touches the branch configured in `branch`.** Nothing about
your tracked branch, or what the next regular apply would do, changes.
(Import is the only operation that does write to the tracked branch -
see "Importing an existing config" below.)

One consequence worth knowing while `dry_run` is on: since nothing is
ever written to live in that mode, a file you add to the repository has
never reached Home Assistant, which from here looks exactly like a file
you deleted live. The drift branch captures it as a removal. The reverse
holds too - a file you delete from the repository is still sitting on
live in this mode, so the drift branch adds it back. Nothing is lost as
long as you read the branch before merging it: your own branch still has
the file, and the agent never opens or merges a pull request itself. A
removal of a file you only just added is this case, not a deletion
somebody made on live.

The pushed branch name is shown in the web UI and on
`sensor.gitops_agent_status` as `last_drift_branch`, so you can open a
pull request from it yourself - the agent never opens one on your
behalf.

The automatic trigger captures a given standing drift only once: if the
same set of drifted files sits unresolved across several reconcile
cycles, only the first cycle pushes a branch. It runs again once the
drift's shape changes (a file is added, removed, or a different set of
paths starts drifting) - editing the SAME file's content again while
everything else stays the same still counts as "the same shape" and is
not re-captured; use the manual button if you want a fresh snapshot at
that exact moment. This dedup is tracked per-agent, not per-branch, and
survives an add-on restart.

Needs a write-capable `git_token`. A push failure (bad credentials,
network issue, branch already exists on the remote from a prior manual
retry) surfaces as a normal error in the recent-activity log; it does
not affect the sync state shown by the state pill, since a commit-back
failure has no bearing on whether your live config still matches the
repository.

## Capturing live changes

Enabled by `capture_live_changes` (default `false`). Without it the file
layer is one-way: the repository is the truth, and an edit you make on
this machine is drift for the next apply to overwrite. With it on, every
drifting file is asked a second question - which SIDE moved - and routed
accordingly.

### How it decides

The agent already knows the repository and the live config disagree; what
it needs is a third reference point. That is the commit it last wrote
live, recorded as `last_good_sha`, or the commit an import made when no
apply has ever run. Reading a file out of that commit gives exactly what
the agent put on disk, so:

| Since that commit | What happens |
|---|---|
| only the repository changed | applied here, as always |
| only this machine changed | committed to the tracked branch |
| both changed | refused in both directions |
| neither, but they differ | left for the next check |

The capture is one commit per cycle, message `capture: sync live home
assistant changes`, pushed to `branch` fast-forward only. It is never
forced. If someone pushed between the agent's fetch and its push, it
re-reads your live files and tries once more on the new tip; if it loses
again it gives up until the next cycle, and their commit survives either
way.

### Conflicts

A file that changed in both places is not something the agent will guess
at. It is skipped entirely - not applied, not captured - its live copy is
pushed to a `gitops/conflict-<UTC timestamp>` branch so nothing is
stranded, and it appears under **Needs your decision** in the web UI.
Every other file in the same cycle proceeds normally.

Resolve one by making the two sides agree: merge the conflict branch,
push the version you want, or edit this machine to match. The next check
notices they agree and clears it. There is no button - clearing a
conflict without resolving it would just move the problem.

### Why this cannot race

A captured file is removed from the apply plan before that plan is
published, so there is no moment where an edit has been decided as yours
and an apply is about to overwrite it. The unattended cycle additionally
holds one lock across both halves, but it is the plan filtering, not the
lock, that makes this true for the web and webhook paths as well. A
capture that fails to push holds those files back too, so a bad token
means "not saved yet" rather than "saved nowhere and then overwritten".

Against someone else pushing to the branch, the fast-forward-only push is
the guarantee - the same rule imports and add-on version records already
follow. Against Home Assistant rewriting a file mid-capture, the commit
re-reads live at the moment it stages, so the repository ends up holding
whatever was actually there.

### What it does not cover

Four limits, none of them incidental:

- **New files are not captured.** The agent compares what the repository
  tracks against what is live. A file you create here that git has never
  seen is invisible to that comparison in both directions. Import is
  still the way to bring one in.
- **`.storage/` is not files.** UI-created helpers, most dashboards and
  the entity registry live there, and it is excluded from syncing
  entirely. Those belong to the `reconcile.*` layers, which stay one-way.
  What this DOES cover is the set Home Assistant writes as real files:
  `automations.yaml`, `scripts.yaml`, `scenes.yaml`, `configuration.yaml`,
  `packages/`, ESPHome configs.
- **`gitops/` is never captured.** The manifests are inputs to the agent,
  not config it syncs.
- **Only a file the agent itself put here can be captured as deleted.** A
  repository holds things that are not meant to live in
  `/homeassistant` - `README.md`, `LICENSE`, `.github/` - and to the
  comparison those look exactly like a file you deleted. Deleting one from
  the branch on that basis would be wrong, so the agent requires that its
  own last apply wrote the file before it will remove it from git.

Encrypted files are handled the same as everywhere else: a captured
`secrets.yaml` is encrypted before it is committed, and the comparison
looks at what a file MEANS rather than its bytes, so SOPS re-encrypting
identical content is not mistaken for an edit.

## Importing an existing config

Ordinary syncing only ever moves files from the repository toward Home
Assistant, and drift capture only sees files the repository already
tracks - so neither can get an existing `/config` into an empty
repository. Import is the one-way trip that can, enabled by
`allow_import` (default `false`).

### An empty repository

A repository with no commits has no branch either, so there is nothing
to fetch and nothing to compare against. The agent reports this as its
own state, `unseeded`, rather than as an error: the dashboard says the
branch does not exist yet and points at Import, the sensor carries
`unseeded`, and no error is recorded. It repeats every interval without
filling the activity feed or the run history - the condition is logged
once, when it starts.

Apply refuses while unseeded, because there is no plan to apply. Import
is the way out: it creates the branch and pushes the first commit, and
the reconcile chained after it picks up from there. A first commit you
push yourself works just as well - the agent starts reconciling on the
next tick with no intervention.

A branch name that is simply mistyped looks identical, which is why the
banner says to check `branch` as well. Credentials that do not work, and
a host that cannot be reached, are still errors.

Two buttons, both manual, neither ever triggered by the interval:

- **Preview** scans the live config tree and lists exactly what
  would be pushed, with the total size and a count of what was passed
  over and why. It runs no git command at all and changes nothing. The
  file list itself is collapsed behind a `files` row you click to open -
  a real config previews a couple of hundred paths, and that is a lot of
  page to scroll past on the way to everything else. Opening it shows
  the first 200 names and a count of the rest; `/status.json` carries
  the list complete, under `import_preview.files`, when you want all of
  them. **Dismiss**, in the card's heading, clears the preview off the
  page without importing anything - press Preview again to bring it
  back. It works while another operation is running, since that is
  usually when you want the card out of the way.
- **Import** does the same scan, then writes those
  files into the repository and pushes them as a single commit with the
  message `import: seed repository from live home assistant config`,
  **directly onto the branch named in `branch`**.

### What is captured

Every regular file under `/homeassistant`, except:

- everything in the "What is never touched" list above (`.storage/`,
  `secrets.yaml`, `*.db*`, `*.log`, `backups/`, `deps/`, `tts/`,
  `.ssh/`, `.cloud/`, `.git/`, and a root-level `gitops/`)
- anything matching the secret patterns: `secrets.yml`, `*.pem`,
  `*.key`, `id_rsa*`, `id_ed25519*`, `.env*`
- symlinks and any other non-regular entry - a symlink is never
  followed, so a `notes.yaml` pointing at something outside your config
  can never be captured under an innocuous name, and a symlinked
  directory is not descended into at all
- anything your repository's own `.gitignore` matches, as long as it
  is not already tracked (`git add` still stages a change to a tracked
  file even if a rule would now ignore it)

**With `age_key` set, `secrets.yaml` is captured too**, encrypted
before it is staged - it is the one path the first two exclusions stop
applying to, and the import writes the managed `.sops.yaml` alongside
it. Everything else on both lists is passed over exactly as before.
An import with a key configured also encrypts the secret-shaped values
in every other YAML, JSON and dotenv file it captures, and fails
without importing anything if one of them cannot be encrypted safely -
see "Secret encryption (SOPS and age)" above.

**That last one is the tuning knob.** If an import captures something
you do not want tracked, add it to the repository's `.gitignore` and
run the import again - there is no separate option for this, and there
does not need to be. The one exception is seeding a branch that does
not exist yet: that starts from an empty tree, so there is no
`.gitignore` in it to honor. Push one to the branch first if you need
it, or import, review, and adjust.

### Size limits

An import refuses outright, before running any git command, if the live
tree holds more than 25000 importable files, more than 400 MB in total,
or any single file over 25 MB. These are sized against a real
HACS-equipped install, where `custom_components/` alone can hold several
thousand files. There is also a backstop at 200000
directory entries visited, which only trips if the config directory is
really a mount point for something much larger. The error names what
blew the budget - the largest files, or the largest directories for a
count breach - so the fix is usually obvious:

```
gitsync: import: refusing to import: total size 412.7 MB exceeds the
100.0 MB limit; largest: media/cam/2026-07-31.mp4 (188.2 MB), ...;
move it out of the config directory and try again
```

**`.gitignore` does not help here.** These limits are measured on the
live filesystem before git is involved at all, so an ignored file still
counts against them - ignoring shapes what gets committed, not what
gets scanned. Move the offender out of the config directory instead.

Nothing is imported when a limit is hit. A partial snapshot that looks
complete would be worse than no snapshot at all.

### What it never does

- **It never deletes anything from the repository.** A repository
  legitimately holds paths that never exist live - `gitops/` manifests,
  a README, CI workflows, `.gitignore` itself - and every excluded path
  is invisible to the scan by design, so an import has no way to tell
  those apart from a file you deleted live. Mirroring live exactly
  would remove all of them.
- **It never makes the agent start deleting your live files.** The
  agent only ever deletes a file in `/homeassistant` that it previously
  wrote there itself, and copying a file *into* the repository does not
  count. Reorganizing your repo after an import can never delete live
  config the agent did not place.
- **It never force-pushes.** If the tracked branch moved on the remote
  between the import's fetch and its push, git refuses the update and
  the import reports "push rejected: the tracked branch moved on the
  remote". Nothing is overwritten; run Check Now and import again.

### Recommended workflow

1. Create the repository. Push your own `.gitignore` first if you have
   one in mind; otherwise the import seeds one - see "What an import
   seeds into `.gitignore`" below.
2. Turn on `allow_import`, restart the add-on.
3. Click **Preview**, read the summary line, and open the `files` row to
   read the list itself.
4. Click **Import** (labelled in full "Import from Home Assistant" in its tooltip).
5. Review the commit in your repository.
6. Turn `allow_import` back off.

A preview that no longer interests you goes away with **Dismiss**; a
successful import clears it too. Neither one changes what an import
would capture - the next Preview rescans the live tree from scratch.

The import time is shown in the web UI and as `last_import_utc` on
`sensor.gitops_agent_status`.

## Webhook trigger

Empty `webhook_secret` (the default) means this listener never starts
at all - no socket is bound on port 8098. Set it to enable `POST
/webhook`, gated by that secret via either an `X-Gitops-Token: <secret>`
header or a `?token=<secret>` query parameter (the header is checked
first if both are present). A match triggers an immediate reconcile
asynchronously and responds `202 Accepted`; a mismatch responds `403`.
The secret must be at least 16 characters - a shorter one is refused
at startup and the listener stays off. After 30 bad tokens within a
minute the endpoint answers `429` for everything until the minute
rolls over, so a guesser gets a real budget rather than a quieter log.

**Prefer the `X-Gitops-Token` header over `?token=`.** A query
parameter routinely ends up written to a reverse proxy's or client's
access logs verbatim, which the header does not - if your git host or
proxy setup lets you choose, configure the webhook to send the secret
as a header, not as part of the URL.

The trigger is fire-and-forget: `202` means the request was accepted,
not that a fresh reconcile is guaranteed to have already run to
completion, or even started, by the time the response is written. If
the agent is already busy with another operation, the webhook's
reconcile request is simply absorbed - nothing is queued, but the
in-progress operation (or the next regular poll tick) picks up any real
change regardless.

This listener is entirely separate from the ingress dashboard on port
8099: Supervisor's ingress proxy is the dashboard's only route in, and
the webhook port is never used for it. A flood of requests with the
wrong token is rate-limited to about one log line per minute so it
cannot spam the add-on's log; every individual request is still
answered `403` regardless of the log line being suppressed or not.

## Add-on auto-update

Off by default. List an add-on's slug in `auto_update_addons` and the
agent installs that add-on's updates as Supervisor reports them. Leave
the option empty and none of this runs at all - there is no check and no
background loop.

This is the one part of the agent that is not about your repository.
Nothing in git says which version of ESPHome should be installed, so an
add-on version cannot be planned as drift, listed as a pending change,
or undone by Rollback. It is a separate loop, on a cadence of its own,
with its own card on the dashboard, and it never moves the sync state.

### What it does and does not do

It installs updates for add-ons you already have and named yourself.
That is the whole of it:

- **It never installs an add-on you do not have.** A slug that is not
  installed is reported as "not installed" and skipped - which is also
  what a typo looks like, so read that row rather than assuming the
  add-on is up to date.
- **It never uninstalls or removes anything**, and it never downgrades.
- **It never updates this agent.** Its own slug is refused whatever the
  list says: updating the add-on that is running the update means
  Supervisor stopping the container mid-call, with nothing left to
  record how it went. Update this one from its own page, where the
  restart is expected. The refusal is shown on the dashboard rather than
  silently dropped, so a slug that is being ignored says so.
- **It never touches Supervisor's own auto-update switch.** The "Auto
  update" toggle on an add-on's page belongs to Supervisor and is left
  exactly as you set it; this option is a separate mechanism that
  installs the update itself. Pick one per add-on rather than both:
  whichever gets there first installs the update, and the loser is most
  likely to end up reporting an error for an update that did land, which
  the next check then corrects. Nothing is damaged by it, but the card
  is easier to read if only one of the two is doing the installing.
- **It never writes to `/homeassistant`** and never restarts Home
  Assistant. Supervisor restarts the add-on it updated, if that add-on
  was running.

### Finding a slug

Entries are slugs, not display names: the last path segment of the
add-on's page URL in Home Assistant. A page at
`/hassio/addon/a0d7b954_esphome/info` means the slug is
`a0d7b954_esphome`; core add-ons look like `core_samba`.

### When it runs

Not on the `interval_minutes` sync tick - this has nothing to do with
the repository, and pinning it to a one-minute poll would ask Supervisor
the same question 1440 times a day. It runs once about two minutes after
the add-on starts, then every `auto_update_interval_minutes` (six hours
by default).

The startup delay is there because add-on startup is the worst moment to
ask: the agent comes up while Supervisor is still starting everything
else on the host, and the first reconcile - the thing you actually watch
after a restart - is competing for the same Supervisor. The startup
delay is fixed; only the repeat interval is configurable.

Six hours is the default rather than a fixed cadence, and it is paced to
Supervisor rather than to this agent. Supervisor
refreshes its own copy of the add-on store on a timer of its own and
answers version queries out of that cached copy, so asking more often
would not learn about a new release any sooner. An update lands within a
working day of appearing.

The **Check for updates** button on the card runs that same check on
demand. It is not a refresh of what is already on screen: with `dry_run`
off it asks Supervisor about every listed slug and installs whatever it
finds, and Supervisor restarts an add-on it updates. That is why the
button asks for confirmation first, the way Apply does. With `dry_run`
on it does not ask, because nothing will be installed.

Two checks never overlap. Press the button while one is running - the
timer's, or your own - and the second press is refused and logged rather
than queued; the button is greyed out for as long as a check is in
flight. It is not greyed out while a reconcile, an apply or an import is
running, though: those are a different lock, and a check is free to
start while one of them is in progress. What it cannot do then is
install - an update it finds is deferred to the next check rather than
queued, and its row says so (see "While a reconcile is running" below).
Press the button again once the operation has finished.

The button also works while the agent is paused, and it still installs.
Pause stops what the add-on does unattended; a button is not that. See
"Pausing automatic checks" below.

### Results survive a restart

Every completed check writes its rows to `/data/addon_updates.json`, the
add-on's own volume, and the next start reads them back. The card is
populated the moment the page loads instead of standing empty for the
two minutes before the first check - which matters most right after an
add-on update or a host reboot, when the question you have is usually
what the agent knew before it went down.

Nothing is re-stamped on the way in. A restored row says when it was
actually checked, which after a reboot may well be hours ago, and the
card marks it stale on exactly that basis. An agent that reset those
timestamps at startup would be claiming a check it never ran.

Slugs are matched to the current `auto_update_addons` list, not to
whatever the file holds. A slug you have since removed from the option
is dropped; a slug you have since added gets a row reading "not checked
yet" until the first check reaches it. If the file is missing, empty or
unreadable - a first run, or a `/data` the agent could not write - the
card simply starts empty, and a check that cannot save its results still
records them and still shows them.

### A backup before every update

Each update is preceded by a partial Supervisor backup of just that
add-on, taken by Supervisor as part of the update call. If a release
turns out to be bad, restore that backup from **Settings > System >
Backups** - the add-on goes back to the version it was on, with its
options and data.

That is the only way back. There is no "downgrade" call for the agent to
make, so an update is not something Rollback can undo the way it undoes
a file or a registry change, and it is deliberately not offered as one.

An update call blocks until Supervisor has taken that backup, pulled the
new image and restarted the add-on, which on a slow disk and a slow link
can take a while - a large image spends most of it in the pull. The
agent waits up to 30 minutes per add-on rather than reporting a failure
for an update that then goes on to land anyway.

### With `dry_run` on

The check is report-only. The dashboard and the log say which of the
listed add-ons have an update waiting and what the two versions are, and
nothing is installed. This is the same switch that governs file and
registry changes, so a dry-run install is not something you can force
from the Apply button either - Apply is about the repository, and an
add-on version is not in it. Turn `dry_run` off when you want the
updates installed.

The log line is written when the versions change, not on every cycle: an
update nobody installs stays available forever, and repeating the same
line every 6 hours would bury everything else in the activity feed.

### When an update fails

A failed check or a failed install is reported and then left alone. It
never sets the sync state to "error".

Every outcome lands on the add-on's row in the card, which always shows
the last verdict for every watched slug. What additionally reaches the
activity feed is the news:

- **A failed install** is logged every time it happens.
- **A failed check** - the agent could not get an answer about that
  add-on out of Supervisor at all - is logged when it starts failing,
  and again when it recovers. Not once per cycle: an unreachable
  Supervisor stays unreachable for hours, and four identical lines a day
  per add-on would push the reconcile history out of the feed. A later
  outage is logged again as its own.
- **"Not installed"** is never logged, on any cycle. Supervisor answered
  the question, and a slug that is a typo does not stop being one; the
  row says so every time you look at it.

That separation is deliberate. The state pill answers one question -
does `/homeassistant` match the repository - and a failed image pull
says nothing about it. Turning the dashboard red would report a config
problem that does not exist, and the next reconcile would clear it
again, hiding the thing that actually failed. The next check tries the
update again in 6 hours.

Supervisor can also report an update as successful without the version
moving. The agent re-reads the add-on afterwards rather than trusting
the call, and says "update did not take" rather than claiming an install
that did not happen.

### While a reconcile is running

The two share a lock, taken per add-on rather than once around the whole
batch. If a reconcile, an apply or an import is already running when an
update is due, that update is deferred - reported as "deferred: another
operation is running" - and installed on the next cycle. Nothing is
queued and nothing is retried in a tight loop.

Which is also why the lock is per add-on. A reconcile that comes due
while an update is in flight is skipped for that tick and runs on the
next one, so it sits out one add-on's image pull rather than the whole
list's.

### What the dashboard shows

An "Add-on updates" card, listing one row per slug in the option - not
one per available update. An add-on that is current, one that is not
installed, one whose check failed and this agent's own refused slug all
get a row, because a row silently missing from the list is exactly how a
typo'd slug stays invisible. Each row carries the versions
(`current -> latest` when there is an update), the last verdict, and
when it was last checked and last updated by the agent.

The check time is shown as an age - "checked 24 min ago" - because the
useful question about a background job is how long ago it last ran, not
what the clock said when it did. Hover it for the exact UTC timestamp.
Once that age passes a whole check interval - 6 hours - the row is
marked **stale** in orange: at that point a check that should have
happened demonstrably did not, and every version on the row is older
evidence than it looks. Rows restored at startup are aged from when
they were really checked, so a box that was off for a day comes back
with a card that says so, and one restarted five minutes ago does not.

Two kinds of row can never change on their own: this agent's own slug,
which it refuses to update, and a slug Supervisor does not have
installed. Those fold into a collapsed **not updatable by this agent**
row at the bottom of the list, with a count, so the add-ons that can
still move are the ones you see first. They are not dropped - they are
still watched, still re-checked on every cycle, and one click away, so a
typo'd slug is still there to be found. To move one back up, install the
add-on, or take the slug out of `auto_update_addons`.

A *failed* check is deliberately not folded, even though it wears the
same grey "unknown" badge. It is the one unknown that is asking you for
something: Supervisor gave no answer about that add-on, and it stays in
the main list until it does.

The **Check for updates** button sits in the card's heading - see "When
it runs" above for what pressing it does, which includes installing.

The card appears as soon as the option is set. Its list is empty only
until the first check has run, or after a start that found nothing saved
from a previous one.

Two attributes on `sensor.gitops_agent_status` carry the same
information for automations: `addon_updates_available`, the number of
listed add-ons with an update waiting, and `last_addon_update`, the
`<slug> <version>` of the most recent one the agent installed.

With `track_addon_versions` on as well, an update the agent installs
turns up in the repository on the next reconcile rather than at the
moment it lands: the record is written by the reconcile cycle, and the
update loop runs on its own six-hour cadence. Normally that is within
one `interval_minutes`, but only a cycle that completes records
anything - see "When it writes" below.

## Recording add-on versions

Off by default. Turn `track_addon_versions` on and every reconcile cycle
ends by writing the versions of the installed add-ons into the
repository:

```yaml
# gitops/addon-versions.yaml
a0d7b954_esphome:
  name: ESPHome Device Builder
  version: 2025.8.0
core_samba:
  name: Samba share
  version: 12.3.2
```

It answers a question the rest of the agent cannot: what was actually
running here. A config repository records what Home Assistant should
look like, but not which version of ESPHome produced the firmware in it,
and after a rebuild - or an update that went badly - that is exactly the
thing nobody wrote down.

### It records, it does not manage

This file is the one thing under `gitops/` the agent WRITES rather than
reads. It is a record, not a manifest:

- **Nothing in it is ever installed.** Editing a version here does not
  downgrade or upgrade anything; the agent never reads the file back.
- **Hand edits are overwritten.** The comparison is byte-exact against
  what the agent would write, not a reading of the versions in it, so
  *any* edit is reverted on the next cycle - a comment you added, a
  reordering, a change in whitespace, as much as a version you corrected.
  The header comment in the file says so, for whoever finds it in a diff.
- **It is never copied into `/homeassistant`.** All of `gitops/` is
  excluded from file sync in both directions, so it also never shows up
  as pending drift.
- **It records what it observes**, not what the agent did. A manual
  update from the Home Assistant UI, Supervisor's own auto-update
  toggle, `auto_update_addons`, and a restored backup all land in it the
  same way.

### When it writes

At the end of every reconcile cycle that ran to completion, so a version
that changed is normally recorded within one `interval_minutes`. That is
the usual case, not a guarantee: a cycle that stopped early records
nothing, and nothing is recorded at all until a cycle completes. Stopping
early is not only about an unreachable repository - a tracked plaintext
secret, a file that could not be decrypted and a manifest that will not
load each end the cycle with the repository working perfectly well.
Whatever ended it is reported in the usual place, and the record resumes
with the first cycle that gets all the way through.

Almost every one of those cycles writes nothing at all. The file is
rendered and compared against what is already committed, and a commit
and push only happen when the bytes differ, so an install where nothing
has been updated produces no commits however often the agent runs. The
rendering is deterministic - add-ons sorted by slug, one shape per entry
- precisely so that "nothing changed" stays byte-identical.

One further case writes nothing: if Supervisor answers with an empty
add-on list, the record is skipped rather than emptied. This agent is
itself an installed add-on, so an empty list is never a truthful answer,
and believing it would blank the file out and fill it back in on the next
cycle. It goes to the add-on log only - no activity event, no commit -
and if a failure was already being reported, it stays reported until a
record actually succeeds.

The commit lands directly on the tracked branch, as
`versions: record installed add-on versions`, authored by the agent. The
push is fast-forward only and never forced, the same as an import: if
someone else pushed in between, the agent re-fetches and tries once more
on the new tip, and gives up until the next cycle if it loses again.
Nothing anyone else pushed is ever overwritten.

`dry_run` does not gate it. That option governs changes to Home
Assistant, and this writes to the repository - the same line
`allow_import` and `commit_back` already draw. `git_token` does need
push rights; without them the push fails.

### What the activity feed shows

On a cycle that committed, one line per add-on whose version moved -
`recorded version change: a0d7b954_esphome 2025.7.1 -> 2025.8.0`, or
`added at` / `removed (was ...)` for one that appeared or went away.
More than five at once (a core update usually brings several) collapse
into the first five plus a count.

The first record after the add-on starts has nothing in memory to
compare against and reports itself as `recorded add-on versions (N
add-on(s))`; the file's own diff has the detail.

A failure - Supervisor unreachable, a token without push rights, the
branch moving under it twice - is written to the feed once, when it
starts failing, and once more when it recovers, rather than on every
cycle for as long as it lasts. It never sets the sync state to "error"
and never fails the reconcile: which versions are installed says nothing
about whether the config matches the repository.

## Secret encryption (SOPS and age)

Off unless `age_key` is set. With a key configured, the agent encrypts
secret values with [SOPS](https://github.com/getsops/sops) before they
enter the git worktree, and decrypts them again on the way back into
your config.

**The live side is always plaintext.** Home Assistant reads
`/homeassistant` exactly as it always did; nothing there is ever
encrypted at rest. The ciphertext exists only in the repository, and
only for the values that need it.

### Values, not whole files

An encrypted config file is still a readable, reviewable config file.
Only the values behind secret-shaped keys become `ENC[...]` strings -
the structure, the comments and every ordinary value stay in the clear,
so a pull request against your config repository is still worth
reading:

```yaml
mqtt:
  broker: 192.168.1.10
  port: 1883
  password: ENC[AES256_GCM,data:Uy4v...,type:str]
```

`secrets.yaml` is the one exception, and it goes the other way: every
value in it is encrypted, because every value in it is a secret by
definition. It is also the file that stops being excluded when
encryption is on - with no `age_key` it is never synced in either
direction, and with one it syncs like any other config file.

### Which files are covered

Three formats, because those are the three SOPS can encrypt one value
at a time:

- **YAML** - `*.yaml` and `*.yml`.
- **JSON** - `*.json`. Google service account keys (a `private_key`
  holding a PEM block) and Zigbee coordinator backups (`key` fields)
  both live in these.
- **dotenv** - `KEY=value` files with **no extension at all**, such as
  the meter definitions wmbusmeters keeps in
  `wmbusmeters/etc/wmbusmeters.d/`, which hold a wM-Bus AES key on a
  `key=` line.

Anything else - a `.py`, an image, a database - is never encrypted and
never touched.

The dotenv rule is deliberately narrow, and a file qualifies only if
**all** of the following hold:

1. its name has no extension,
2. every non-blank, non-`#` line is a `KEY=value` assignment,
3. at least one of those keys is secret-shaped by the rule below.

The reason for the narrowness is what happens when the guess is wrong.
SOPS picks how to read a file from its extension, and a file it cannot
place falls back to the **binary** store: the entire file is base64'd
into one opaque `data` field and the per-value rule is discarded. That
turns a reviewable config file into an unreadable blob. Its matching is
also case-sensitive, so a file saved as `config.JSON` is one it cannot
place.

The agent therefore never lets SOPS guess: it works out the format
itself and names it on every call, in both directions. A file whose
format it cannot establish is left alone rather than encrypted, and an
extensionless file has to look like a secrets-bearing dotenv file in
its own content before it is treated as one. A `.env` file is
not covered here at all: those are refused outright as secret-shaped
paths, encryption or not, along with `*.pem`, `*.key` and `id_rsa*`.

### Which keys count as secrets

A mapping key anywhere in a YAML or JSON file, at any depth - or the
key half of a dotenv assignment - whose name is one of `password`,
`passwd`, `pwd`, `secret`, `secrets`, `token`, `credential`,
`credentials`, `auth`, `authorization`, `psk`, `key`, `keys`, `apikey`
or `api_key`.

A key also matches when it ends in `password`, `passwd`, `pwd`,
`secret`, `token`, `key`, `keys`, `credential`, `credentials`, `psk`
or `auth` as a full, underscore-separated suffix - so
`mqtt_password`, `client_secret` and `network_key` all match. Note the
suffix list is shorter than the whole-key list above: `client_secrets`
and `x_authorization` do **not** match, because their suffixes
(`secrets`, `authorization`) are only recognized as whole keys. If you
use a name like that, rename it or move the value into
`secrets.yaml`.

The exact rule, which is also what gets written into `.sops.yaml`:

```
(?i)^(password|passwd|pwd|secret|secrets|token|credential|credentials|auth|authorization|psk|keys?|api_?key|.*_(password|passwd|pwd|secret|token|keys?|credential|credentials|psk|auth))$
```

Whole-key matching, not substring matching, is deliberate: `monkey`
and `keyboard` are not secrets, and a rule that treated them as ones
would encrypt half a config.

`pin` is deliberately absent, in both forms. In this domain a "pin" is
a GPIO number - an ESPHome config is full of `pin: GPIO4`, `cs_pin`
and `i2s_bclk_pin` - and encrypting hardware wiring would make those
files unreadable for no gain. Put a real PIN code in `secrets.yaml`,
where every value is encrypted whatever its key is called.

A matching key only triggers encryption when its value is real secret
material - a plain scalar, or a list of them. A `password: !secret
mqtt_pw` is a reference, not a secret, and is left alone; so is a
`auth:` that opens a nested block rather than holding a value.

### Files the agent refuses to encrypt

Some files hold a secret that SOPS cannot encrypt without breaking
something. Each is refused outright, naming the file and the fix,
rather than being encrypted anyway or committed in the clear.

The common one is a file that holds an inline secret **and** a Home
Assistant custom tag (`!secret`, `!include`,
`!include_dir_merge_list`, `!input`, or any other single-`!` tag):

```
configuration.yaml holds a secret value inline alongside a Home
Assistant custom tag (!secret, !include..., !input), which SOPS cannot
encrypt without destroying the tag: move that value into secrets.yaml
and reference it with !secret
```

This is not caution for its own sake. SOPS rewrites the whole document
when it encrypts, and it does not round-trip custom tags: a `!secret
mqtt_pw` node comes back as an ordinary encrypted string with the tag
gone, which breaks the config the next time Home Assistant loads it.
Encrypting anyway would corrupt your config; skipping the file would
push the secret in the clear. Neither is a choice to make silently, so
the operation fails and tells you the fix - move that one value into
`secrets.yaml` and reference it with `!secret`.

The same refusal, with the same fix, covers three more shapes:

- **A top-level list.** `automations.yaml` is always one, and SOPS
  only encrypts a document whose root is a mapping.
- **An unquoted `yes`, `no`, `on` or `off` anywhere in a file that is
  encrypted key-by-key.** SOPS re-writes the whole document, quoting
  those, and Home Assistant then reads a string where it read a
  boolean - a config change you never made. Quote the value yourself
  and it is fine.
- **A literal top-level `sops:` key**, which SOPS reserves for its own
  metadata.

A file that does not parse as YAML is refused too if anything in it
looks secret-shaped, since the agent can neither encrypt it nor clear
it for the remote.

JSON and dotenv files have their own short lists, for the same reason -
each is a shape SOPS rejects:

- **A JSON file whose top level is not an object.** SOPS only encrypts
  a JSON document that starts with `{`. Wrap an array in an object and
  it is fine.
- **A JSON file with a top-level `"sops"` key**, or a dotenv file with
  a key starting with `sops_`. SOPS keeps its metadata there, and in a
  format that cannot nest - dotenv - it uses the whole `sops_` prefix.
  A file that already has one would also be mistaken for an
  already-encrypted file, and committed untouched. Rename the key.
- **A file that does not parse as the format its name claims** - a
  `.json` that is really YAML, say - if anything in it looks
  secret-shaped.

### What an encrypted file's diff looks like

Diffs on the dashboard, on `GET /status.json` and on
`sensor.gitops_agent_status` have their secret values masked before
they are published. For an encrypted YAML file that means a real
unified diff with the secret values replaced by `*****`, so an ordinary
edit next to a secret is still reviewable.

For an encrypted JSON or dotenv file the whole diff collapses to
`encrypted values changed (hidden)` instead. The masking pass reads
YAML and only YAML, and running it over a different grammar would mean
deciding which lines are safe to publish using rules for a language the
file is not written in. Hiding the diff is the safe answer; the change
is still reported, just without its contents.

### The managed `.sops.yaml`

The agent writes a `.sops.yaml` at the repository root carrying its own
recipient and one rule per covered path: `secrets.yaml` whole, other
YAML files per value, JSON files per value. It exists so that `sops
secrets.yaml` in your own clone gives you exactly the treatment the
agent applies, rather than something subtly different.

**dotenv files have no rule, and cannot have one.** A creation rule is
matched on the path, and these files have no extension to match; a rule
broad enough to catch them would also make a bare `sops encrypt
meter-0001` succeed by binary-encrypting the whole file, since nothing
in a `.sops.yaml` can set the input type. Without a rule that command
fails with "no matching creation rules found", which is the answer you
want. To edit one by hand, name the format and the rules yourself:

```
sops decrypt --input-type dotenv --output-type dotenv \
  wmbusmeters/etc/wmbusmeters.d/meter-0001

sops encrypt --in-place --input-type dotenv --output-type dotenv \
  --age age1... --encrypted-regex '(?i)^(password|...|api_?key|...)$' \
  wmbusmeters/etc/wmbusmeters.d/meter-0001
```

The recipient is the `age:` value in the managed `.sops.yaml`, and the
regex is its `encrypted_regex`. Leaving out `--input-type dotenv` on
either call is the mistake to avoid: on decrypt it fails loudly, but on
encrypt it silently produces a whole-file binary blob.

Two things follow from it being managed:

- **Its `creation_rules` are regenerated.** Edit them and the next
  import or drift commit puts them back. Anything else you add to the
  file is left alone.
- **The agent's own calls do not consult it.** Every `sops` call the
  agent makes carries its rules on the command line *and* runs from an
  empty directory outside the checkout, so no `.sops.yaml` in the
  repository - not the managed one, and not one added at any depth by
  you, by a merge, or by another tool - can change what the agent
  encrypts or who it encrypts to. Both halves matter: `sops` finds its
  config by searching upward from its working directory, and a rule
  file reachable that way could otherwise switch encryption off for a
  path while still producing a file that looks encrypted.

The file is excluded from sync, so it is never written into
`/homeassistant` and never shows up as drift.

### Generating the key

Any age implementation will do; `age-keygen` is the usual one:

```
age-keygen -o gitops-agent-key.txt
```

That writes a public recipient (`age1...`) and a private identity
(`AGE-SECRET-KEY-1...`). Paste the private identity into `age_key`.
The public half needs no configuration - the agent derives it.

**Keep a copy of the private key somewhere outside Home Assistant.**
If you lose it, the encrypted values in your repository cannot be
recovered by anyone, including you. It is the only thing that can read
them. A password manager entry or an offline copy is enough; what you
must not rely on is the add-on's own options being the only copy.

### Editing secrets by hand

With the managed `.sops.yaml` in place and your private key available
to `sops` (usually via `SOPS_AGE_KEY_FILE` pointing at the file
`age-keygen` wrote), an ordinary clone edits like any other:

```
sops secrets.yaml       # opens decrypted, re-encrypts on save
sops decrypt secrets.yaml
```

Commit and push the result. The agent decrypts it on the next cycle
and applies the plaintext to `/homeassistant`.

### When the key is wrong or missing

Both fail loudly, and nothing is applied:

- **No `age_key`, encrypted content in the repository.** The cycle
  stops with "repository contains SOPS-encrypted files but no age_key
  is configured". The agent will not compare, and will not write,
  content it cannot read - copying `ENC[...]` strings into your config
  would be worse than doing nothing.
- **The wrong `age_key`.** Decryption fails, the cycle stops, and the
  error names the file. Nothing partial is applied: the alternative
  would be a config where the files the agent understood were updated
  and the ones it did not were quietly left behind.

A tracked `secrets.yaml` that turns out **not** to be encrypted is
refused too, before anything is checked out - the agent reads the blob
out of the object database to decide, so a plaintext secret in the
repository is never written to disk on its way to being rejected.

### What the dashboard shows

The pending diff for an encrypted file is masked before it is
published. Both sides are masked, not just the repository's, because
the live side holds the same secrets: every secret value is replaced
with `*****`, and if masking cannot be done confidently the file's
diff collapses to `encrypted values changed (hidden)`. The same masked
text is what reaches `GET /status.json` and the
`sensor.gitops_agent_status` attributes.

### What is still plaintext

- **`/homeassistant`**, always - see above.
- **The pre-apply backups under `/data/backup/<timestamp>/`.** They
  are copies of your live files, taken before they are overwritten, so
  Rollback can restore exactly what was there. That includes a
  plaintext `secrets.yaml`. `/data` is this add-on's own Supervisor
  volume, not shared with anything else, and it is the same place the
  private key itself lives.
- **`gitops/` manifests.** The registry, dashboard, add-on option,
  integration, subentry and HACS manifests are agent input rather than
  Home Assistant config, and this version does not encrypt them. Do not
  put a secret in one - use a `secret://<name>` reference instead, which
  the three manifests carrying data payloads support (see "Referencing
  secrets" above). `gitops/hacs.yaml` has no data payload at all: an id, a
  repository, a category and a version are public by construction.
- **The run history at `/data/history.jsonl`.** Commit SHAs,
  timestamps, counts and the text of any error a run reported. No file
  contents and no secret values: the error text comes from
  `check_config` and from Home Assistant's own API, which name entities
  and files rather than quoting them. It lives on the same add-on-private
  volume as everything above.
- **The add-on update results at `/data/addon_updates.json`.** The
  slugs you listed in `auto_update_addons`, their display names and
  versions, the verdict text of the last check and its timestamps. All
  of it is Supervisor's own answer about an add-on, which is why it can
  be written back out as-is; nothing from a manifest and no path goes
  into it. Same add-on-private volume as `state.json`, the run history
  and the pause flag.

## Safety model

- **Dry-run by default.** No file in `/homeassistant` is touched until
  you either set `dry_run: false` or click Apply in the web UI. The
  same applies to registry changes once `reconcile.registries` is on,
  to dashboard changes once `reconcile.dashboards` is on, to add-on
  option changes once `reconcile.addon_options` is on, to integration
  changes once `reconcile.integrations` is on, to subentry changes once
  `reconcile.subentries` is on, and to HACS downloads once
  `reconcile.hacs` is on - no setup flow is ever started, no config entry
  is ever deleted, and no code is ever downloaded, until an apply actually
  runs.
- **Validate before apply.** Every apply calls the Supervisor's
  `check_config` endpoint before touching anything permanently; on a
  failed check every changed file is rolled back to its pre-apply
  state.
- **Warnings are surfaced, not swallowed.** A successful `check_config`
  can still report warnings (for example, a typo'd integration name
  that Home Assistant silently skips instead of loading). These never
  block the apply or change the sync state - the agent already treats
  the config as valid - but the verbatim warning text is shown in a
  callout on the dashboard and pushed as the `warnings` attribute on
  `sensor.gitops_agent_status`, so a change that "applied cleanly" but
  didn't do what you expected is not invisible.
- **Backups before every apply.** Touched files are copied to
  `/data/backup/<timestamp>/` before being overwritten, and the agent
  requests a partial Supervisor backup before applying. Registry
  changes get the same treatment: every floor, area, label, or helper
  object about to be touched is snapshotted first, and so is every
  entity customization, every dashboard's prior metadata and view
  config, every add-on's prior option values, and every integration
  about to be created, adopted, or deleted, so Rollback in the web UI
  undoes registry, entity, dashboard, add-on option, and integration
  changes alongside file changes - with one caveat unique to
  integrations: rolling back a deletion re-creates the integration by
  re-running its setup flow, which gets it a new identity rather than
  restoring the exact one that was removed (see "Ownership
  (integrations)" above).

  **Two layers stash nothing, and Rollback leaves them alone: subentries
  and HACS.** A subentry's previous data cannot be read back, so there is
  nothing to restore it from; a HACS download's only inverse would be an
  uninstall, which this add-on never does. An apply made of nothing but
  those two says so in the Roll Back dialog rather than promising to undo
  something it cannot.

  The two backups are independent, and only one of them is required. The
  copies under `/data/backup/<timestamp>/` are what the Rollback button
  restores from; taking them is part of the apply, and if it fails the
  apply fails with it. The Supervisor backup is a second net for the case
  where this add-on itself is broken or gone, and it is best-effort: the
  apply goes ahead whether or not Supervisor produced one.

  Best-effort does not mean silent. That request is synchronous - it holds
  the connection open until Supervisor has finished writing the archive -
  and `homeassistant: true` covers the whole core config directory,
  recorder database included. On a large install that can take a while,
  which is why the agent waits up to 15 minutes for it. If it fails
  anyway, the apply still completes and Rollback still works, but the
  dashboard says so in a "Pre-apply backup did not run" callout carrying
  the reason, rather than leaving you to find it in the add-on log.
- **Deletion is scoped.** The agent only deletes a file in
  `/homeassistant` if that exact file was previously applied by the
  agent itself
  (tracked in `/data/state.json`). It never deletes a file it did not
  create. The same scoping applies to registry objects, dashboards and
  integrations - see "Ownership (floors, areas, labels, helpers)",
  "Ownership (dashboards)" and "Ownership (integrations)" above - and, in
  its own update-only way, to entities and add-on options - see
  "Ownership" under `gitops/entities.yaml` and "Ownership (add-ons)"
  above. Two layers never delete anything at all, whatever the repository
  says: removing a subentry from `gitops/subentries.yaml` or a repository
  from `gitops/hacs.yaml` only stops the agent following it (see
  "Ownership (subentries)" and "Ownership (HACS)" above).
- **Import is opt-in, manual and additive.** The one operation that
  writes to your tracked branch is off by default (`allow_import`),
  never runs on the interval, only ever happens on a button click,
  pushes fast-forward only and never forced, never removes anything
  from the repository, and refuses outright rather than truncating when
  the live tree is bigger than its limits. It also never adds the
  imported paths to the agent's own manifest, so importing a file never
  grants the agent permission to delete it from live later.
- **Secrets are never synced in plaintext, and their presence is
  rejected.** With no `age_key` configured, secrets are not synced at
  all: if the source repository tracks anything that looks like one
  (`secrets.yaml`, private keys, `.env`, etc.) the agent refuses to
  sync rather than risk exposing or overwriting it, and an import
  passes those paths over instead of pushing them.

  With an `age_key` configured, exactly one of them earns a different
  answer: a `secrets.yaml` (or `secrets.yml`) that is genuinely
  SOPS-encrypted is synced like any other file. A plaintext one is
  still refused, and so is everything else on that list - a `*.pem`, a
  `*.key`, an `id_rsa`, a `.env` is raw key material with no reason to
  be in a config repository, encrypted or not. See "Secret encryption
  (SOPS and age)" above.

## What is never touched

Regardless of options, the following are excluded from every
comparison and every apply, in both directions:

- `.storage/` (Home Assistant's internal registries)
- `.cloud/`
- `secrets.yaml` - **unless `age_key` is set**, which is the one entry
  on this list encryption changes: it then syncs like any other config
  file, as ciphertext in git and plaintext live. Everything else here
  is excluded unconditionally.
- `.sops.yaml` (the agent's own managed sops config - repository
  tooling, never Home Assistant config)
- `*.db`, `*.db-*`, `*.db.*` (the recorder database, its WAL/SHM
  sidecars, and suffixed copies such as a Zigbee2MQTT `database.db.backup`)
- `*.log`, `*.log.*`
- `.ssh/`
- `deps/`
- `backups/`
- `tts/`
- `.git/`
- `__pycache__/`, `*.pyc`, `*.pyo` (Python bytecode, rewritten whenever
  the interpreter reloads the module beside it)
- `.cache/`, `.HA_VERSION`, `.uuid`, `.ha_run.lock` (instance identity
  and run state)
- `ip_bans.yaml`, `known_devices.yaml`, `known_devices.yaml.bak`
  (written by Home Assistant, not by you)
- `.esphome/` and `.device-builder*` (ESPHome's build cache and the
  Device Builder add-on's state, which includes binary key material)
- `image/` at the config root only - Home Assistant's uploaded-image
  store. A `www/image/` folder of your own is yours and keeps syncing.

Everything on that list is machine-written state rather than
configuration: it comes back on its own, and syncing it costs a diff
every time Home Assistant touches it.

### What an import seeds into `.gitignore`

A repository with no `.gitignore` gets one on its first import, listing
the paths that some other tool owns:

- `custom_components/` and `www/community/` - installed and updated by
  HACS. Tracking them means a several-thousand-file diff per update, and
  an apply would roll an update back to the committed copy.
- `zigbee2mqtt/state.json`, `zigbee2mqtt/database.db*`,
  `zigbee2mqtt/coordinator_backup.json` - rewritten while the add-on runs
- `node-red/flows_cred.json`, `node-red/.config.*.json`
- `appdaemon/compiled/`
- `*.bak`, `.DS_Store`

This is a starting point, not policy: it is written once and never
rewritten, so deleting a line and importing again is how you start
managing something on it. That is the difference between this list and
the one above - those paths are refused unconditionally because managing
them is unsafe, these are merely noisy.

## Ingress web UI

The add-on exposes a small ingress panel ("GitOps") showing current
sync status, the pending diff (when `dry_run` is on), and buttons to
trigger a manual reconcile, apply the current diff, or roll back to
the last known-good state. A "Commit Back" button also appears
whenever `commit_back` is enabled and there is pending file drift - see
"Drift commit-back" above. "Preview" and "Import" appear whenever
`allow_import` is enabled, regardless of
whether anything is pending - see "Importing an existing config" above.

### Watching an operation from a script

The action routes answer immediately - an apply can wait 15 minutes on
the pre-apply backup, far past any HTTP timeout - so the response says
only that the operation was accepted, never how it went. A browser polls
the page for that; a script has `GET /status.json`.

Each accepted POST returns an `X-GitOps-Op-Id` header, and
`/status.json` carries a matching `operation` object:

```json
"operation": {
  "id": 7,
  "name": "import",
  "running": false,
  "error": "",
  "finished_utc": "2026-08-06T09:44:10Z"
}
```

Read the id from the POST, then poll until `operation.id` is at least
that id and `operation.running` is false; `operation.error` is then the
outcome, empty on success. The id is what makes this reliable - `busy`
alone cannot distinguish "not started yet" from "already finished", and
can be true because of an unrelated interval tick.

A POST refused because something else is already running returns
`X-GitOps-Op-Refused: busy`, no id, and starts nothing.

The panel does not appear in the sidebar automatically. After
installing, open the add-on's page and turn on **Show in sidebar**
(the toggle next to its icon) - this is a per-install preference set
from that page, not something the add-on can turn on for you. Until
you flip it, the dashboard is still reachable from **Settings >
Add-ons > GitOps Agent > Open Web UI**.

### Restart reminder

When `reconcile.hacs` has just downloaded a custom integration, the header
carries a neutral chip naming what it downloaded: *restart Home Assistant
to load: anker_solix*. Nothing is failing and nothing is being retried -
Home Assistant imports `custom_components/` at startup, so the code is on
disk and not yet loaded. The add-on never restarts Home Assistant itself;
the chip goes away on the first check after the domain comes up. See "HACS
manifest" above.

### Pausing automatic checks

**Pause** stops everything the add-on does on its own, and
nothing that you do. While it is on it runs no cycle: it does not fetch
the repository, does not plan anything, and - with `dry_run` off - does
not apply anything. It also stops the separate `auto_update_addons`
timer, so no add-on is backed up, updated or restarted behind your back
either; both resume together. The header carries a **paused** chip, a
banner says so above the cards, and the "next check in ~4m" countdown is
gone, because there is no next check to count down to.

A pause takes effect immediately, including in the middle of a cycle
that has already started. That cycle finishes its check - so the pending
list you are looking at is complete - but the automatic apply it would
otherwise have made does not happen.

Every button keeps working. **Check Now**, **Apply**, **Roll Back**,
**Retry** and the import buttons all do exactly what they normally do,
and Pause is the one button that is never greyed out while an operation
is running - stopping the timer in the middle of an apply you did not
expect is the case it exists for. This is "suspend", not a kill switch:
the reason to pause is almost always to take manual control for a while,
not to stop using the add-on.

**Check for updates** keeps working too, and it is the one to know
about: with `dry_run` off it installs what it finds and Supervisor
restarts the add-on it updated, paused or not. That follows from what
pause is - it stops the six-hour timer, not you - but an add-on
restarting while the header says "paused" is a surprise worth spelling
out rather than leaving to be discovered.

**Webhook triggers keep working too**, and they are safe while paused
for a reason worth stating: a webhook only ever asks for a *check* (see
"Webhook trigger" above). It runs a reconcile and reports what it found;
it never applies. Nothing reaches your Home Assistant configuration
through it, paused or not.

One thing pause does **not** cover: **a repository write from a check
that something asked for.** With `commit_back` (plus `dry_run`),
`track_addon_versions` or `capture_live_changes` on, any reconcile that
still runs while paused - one you pressed, or one a webhook triggered -
can push a drift branch, an add-on version commit or a capture at the end
of it. Those go to the repository. Nothing reaches your Home Assistant
configuration.

**Pause survives a restart.** The flag lives in the add-on's own data
volume, at `/data/paused` - present means paused - so an add-on restart,
an update or a host reboot comes back paused, and the activity feed says
so on its first line. That is deliberate: a restart is exactly when an
unattended apply would otherwise start again with nobody watching. Press
**Resume** to clear it; the next cycle runs immediately rather
than waiting out the interval.

Automations can read it: `sensor.gitops_agent_status` carries a `paused`
attribute alongside the rest. It is an attribute rather than a state on
purpose - the sensor's state stays `in_sync`, `drift_pending` or
whatever it was, because pause says nothing about whether your live
config matches the repository. An automation that should stay quiet
while the agent is paused checks the attribute:

```yaml
condition:
  - condition: template
    value_template: "{{ state_attr('sensor.gitops_agent_status', 'paused') == false }}"
```

Written as `== false` rather than `not ...` on purpose. If the sensor is
missing or unavailable the attribute reads as `none`, and `not none` is
true - so the `not` form would let the automation run in exactly the
case where it knows nothing. The add-on keeps pushing the sensor on its
normal interval while paused, so this stays fresh.

### Managed by this agent

A read-only card listing everything the add-on currently owns, in eight
groups: files, floors/areas/labels/helpers, entities, dashboards, add-on
options, integrations, subentries and HACS integrations. Names only - no
option values, no flow data, no hashes.

The HACS group is the one whose names the add-on will never act on again
by itself: that layer installs and adopts and never removes (see
"Ownership (HACS)" above). It is listed because the record is the only
place it shows that this add-on, rather than somebody at the HACS panel,
put the integration there.

**Only what is listed there is ever deleted or restored by this add-on.**
That is what the card is for: before you remove something from the
repository, it tells you whether the removal will un-manage a live object
or do nothing at all. Everything else on your Home Assistant instance is
outside the agent's reach, whatever the repository says.

Two things it deliberately does not mean:

- **Files under management are files an apply WROTE, not files that
  sync.** Importing an existing config seeds the repository without
  claiming any of it (see "What it never does" above), so a freshly
  imported install syncs hundreds of files and manages none of them until
  an apply touches them. The card says "Nothing written yet" then, and it
  is telling the truth.
- **A record outlives the `reconcile` option that made it.** Switching a
  layer off stops it being planned; it does not un-manage what it already
  owns, and those objects stay listed here. Nothing acts on them until the
  layer is turned back on, and un-managing them properly means turning it
  back on and removing them from the manifest.

Groups longer than 200 names are cut off with a count of the rest - the
whole list is always in `/status.json` under `managed`.

### Run history

Below the pending changes, the panel shows one row per operation that
actually ran: when it started, what kind it was, the commit it worked
from, how it ended, what it counted, and how long it took. A row that
ended badly carries the reason underneath it.

**This survives a restart.** The activity feed below it does not - that
is the difference between the two cards, and the reason both exist. The
feed is the running commentary of the process you are looking at, wiped
when the add-on restarts or updates; the history is the durable index,
kept in `/data/history.jsonl` and read back at startup. Coming back to a
config that changed overnight, the history is what can still tell you
which run changed it and from which commit.

Four kinds of run are recorded - `reconcile`, `apply`, `rollback` and
`import` - and each ends in one of six outcomes:

- **`in sync`** - a reconcile found nothing to do.
- **`drift`** - a reconcile found changes, now pending.
- **`ok`** - the operation did what it set out to do.
- **`partial`** - part of the plan is live and part of it is not.
  Usually the files applied and a registry layer then failed, or the
  whole apply went through and only the bookkeeping write afterwards did
  not. Deliberately not called an error: "error" beside "6 file(s)"
  would suggest those six files are not in your config when they are.
- **`rolled back`** - it failed, and everything it had done was undone.
  Only ever claimed when nothing at all was left behind.
- **`error`** - it stopped, having changed nothing.

**Refusals are not rows.** An apply that was declined never ran, whether
it was declined because `dry_run` is on, because another operation was
already going, or because the last reconcile failed. Recording those
would add a row every interval for as long as the condition lasts, which
is how a card stops being read. They go to the activity feed instead,
which is where a refusal belongs.

The counts mean what the kind implies: planned for a reconcile, applied
for an apply, restored for a rollback, committed for an import. A
rollback shows no commit, because it moves the config away from one
rather than towards it.

The file keeps the newest 200 runs and the card shows the newest 25;
neither is configurable today (see Roadmap). When there are more, the
card's heading carries an **all 200** link to a page of its own showing
every run the add-on is holding. That page is a snapshot rather than a
live view - it does not refresh itself, so reload it to pick up runs that
have landed since. The file behind both is plain newline-delimited JSON,
so `jq -s . < /data/history.jsonl` reads the whole of it over Samba or
SSH.

## Roadmap

Things this add-on does not do yet, listed so it is clear they are known
rather than overlooked. None of them is a promise, and the shape of any
of them may change:

- **Home Assistant Core and OS updates, surfaced but not installed.**
  Report-only: the dashboard would say a Core or OS update is waiting,
  the same way it already does for add-ons, without ever installing one.
  A Core update restarts Home Assistant and an OS update reboots the
  host, and neither is something an unattended agent should decide.
- **Configurable retention counts.** A successful apply already prunes
  both the per-apply stash directories under `/data/backup/` and the
  Supervisor backups it took, keeping the 5 newest of each (plus
  whichever stash the Rollback button still points at, which is never
  removed even when it has aged out), and the run history keeps its
  newest 200 rows. What is missing is a way to choose those numbers:
  they are baked in, and a small `/data` volume or a long retention
  policy has no say in them.
- **Add-on lifecycle fields in `gitops/addons.yaml`.** The add-on
  options manifest syncs options only; `boot`, `watchdog` and
  Supervisor's own `auto_update` toggle are add-on settings that a
  repository has no way to declare today.
- **Installing an add-on from a manifest.** The manifest can configure
  an add-on that exists and `auto_update_addons` can update one; neither
  can add one to an install that does not have it.
