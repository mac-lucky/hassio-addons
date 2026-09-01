# Changelog

All notable changes to this add-on are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

Nothing yet.

## [0.6.4] - 2026-09-02

A hardening release: a full audit of the agent produced fixes across the
sync engine, the registry layers and the web dashboard. No new options and
no changed defaults; the one behavior change to note is that a
`webhook_secret` shorter than 16 characters now keeps the webhook listener
off (see Security below).

### Fixed

- A transient `git diff` failure during the capture phase no longer wipes
  the conflict record. Paths the agent had refused to sync in either
  direction could be routed back into the apply on the strength of one
  failed diff, overwriting the live edit the record was protecting. The
  error path now keeps every standing conflict out of the apply.
- The Roll Back button survives a restart. The pointer to the newest
  backup stash was memory-only, so restarting the add-on - exactly how
  people try to recover from a bad apply - left the stash directories on
  disk with no way to use them. The pointer is persisted now, on failed
  applies too.
- Rolling back an adopted area, dashboard or other registry object now
  releases ownership again. Ownership used to stick after the rollback, so
  removing the item from the manifest later deleted an object the user had
  made by hand.
- Rolling back files now asks Home Assistant to reload (or restart, per
  `apply_after_pull`), so the runtime matches the restored files instead of
  keeping the applied config in memory.
- A symlinked config file no longer fails every apply. The path is skipped
  with a note and the rest of the batch still applies.
- Two hangs that could wedge the agent until a restart (and one past it):
  a timed-out git call whose helper process lingered could block forever,
  and a backup directory replaced by a file spun the stash allocator in a
  loop with the operation lock held. Restart polls and health probes also
  stop burning their whole deadline when the operation is cancelled.
- Failed applies no longer leak one backup stash directory per check
  interval, and interrupted rollbacks can no longer lose journal entries -
  a stash bookkeeping failure used to drop an entry from the journal
  without undoing it, putting it out of reach of every later retry.
- Add-on options and config entries that vanished between planning and
  applying are handled cleanly instead of jamming their own rollback.
- A subentry created successfully but not identifiable afterwards is now
  adopted by its `match` rules on the next check instead of being stranded
  by the failure memory.
- The whole HACS store listing gets its own timeout, so a slow box no
  longer records it as a transport failure and refetches every cycle.
- `state.json` and the run history are written with an fsync before the
  rename, so a power cut can no longer leave a zero-length file that
  silently resets what the agent manages.

### Security

- Credentials embedded in `repo_url` (`https://user:token@...`) were
  written to the add-on log by two startup messages and could be echoed
  back in git's own errors. Both are scrubbed now, and the dashboard's
  redaction handles more URL shapes.
- Live values read back from Home Assistant - a prior add-on password
  captured when an option came under management, a reconfigure flow's
  current values - could reach world-readable files or unredacted errors.
  `state.json`, the run history and commit-back staging are 0600 now, and
  flow rejections are scrubbed against everything the step submitted.
- The dashboard's state-changing endpoints reject browser cross-site
  requests, and the retry endpoint bounds its input instead of letting an
  oversized value flood the activity feed.
- The webhook listener requires a secret of at least 16 characters and
  locks out after 30 bad tokens per minute. A short secret is refused at
  startup with an error in the log.
- The status sensor's error and warning attributes are capped: sensor
  attributes are visible to every Home Assistant user and exporter, a
  wider audience than the add-on's own dashboard.
- Boolean options degrade safely: a malformed value like the string
  "false" now reads as the option's default instead of enabling it.

## [0.6.3] - 2026-08-28

### Security

- The image runs `apk upgrade` at build time now. Alpine published openssl
  3.5.8-r0 on 2026-08-27, fixing six high-severity CVEs in libssl3 and
  libcrypto3, and the add-on base image had not been rebuilt against it yet.
  Upgrading during the build takes the fix without waiting on the base.

## [0.6.2] - 2026-08-07

### Fixed

- A merge base that a force-push left behind is no longer used. Rewriting
  the tracked branch leaves the old commit in the object database, so it
  still read as present, but it now sits on an abandoned line: comparing
  it with the tip reported everything that differed across the divergence
  as a repository change, and any of those files also edited here turned
  into a conflict that never happened. A base now has to be a commit the
  tip actually descends from. Found on hardware.

## [0.6.1] - 2026-08-07

### Changed

- The health chip and event raised when an import cannot record itself now
  say what else that costs: the record is the merge base capture compares
  against, so on an agent that has never applied, losing it quietly leaves
  the file layer one-way. Nothing else reported that.

## [0.6.0] - 2026-08-07

### Added

- `capture_live_changes`, off by default. With it on the file layer stops
  being one-way: an edit you make on the machine itself, in the Home
  Assistant UI or over SSH, is committed back to the tracked branch
  instead of being overwritten by the next apply.

  Every cycle each drifting file is compared three ways rather than two -
  against the repository, against the live config, and against the commit
  this agent last wrote live. That third reference is what makes the
  decision possible: a file only the repository moved is applied here as
  before, one only this machine moved is pushed to the tracked branch as
  a `capture:` commit, and one that moved in both places is left untouched
  in both directions. The push is fast-forward only and never forced, the
  same rule imports and the version record already follow; a concurrent
  push costs one retry on the new tip, and a rejection holds those files
  back from the apply rather than overwriting the edits it just failed to
  save.

  A file that moved on both sides is a conflict, and conflicts are not
  guessed at. The live copy is preserved on a `gitops/conflict-<UTC
  timestamp>` branch, the path appears under "Needs your decision" in the
  dashboard, and nothing touches it until the two sides agree again - by
  a merge, a push, or an edit here - at which point it clears itself. The
  branch is pushed once per distinct conflict set rather than once per
  cycle, so a conflict left standing does not fill the repository with
  branches or the activity feed with repeats.

  Four things it deliberately does not cover. A file the repository has
  never tracked is invisible to the comparison in both directions, so
  bringing a new one in is still Import's job. What Home Assistant keeps
  in `.storage/` - UI-created helpers, most dashboards, the entity
  registry - is not files and stays with the `reconcile.*` layers.
  `gitops/` is never captured, since those manifests are input to the
  agent rather than config it syncs. And a file is only ever captured as
  deleted if this agent's own last apply put it there: a repository holds
  things that are not meant to live in `/homeassistant` at all, such as
  `README.md` and `.github/`, and to the comparison those look exactly
  like something you deleted.

  `dry_run` does not gate it, the line `allow_import`, `commit_back` and
  `track_addon_versions` already draw: that option governs writes to Home
  Assistant, and this writes to the repository. With `dry_run` on it is
  the most cautious mode the add-on has - live edits flow into git and
  nothing ever flows back. It does need a merge base to compare against,
  and one run of Import or one apply is what creates one, so under
  `dry_run` an import has to come first. `git_token` needs push rights on
  the tracked branch; without them the failure is reported and the
  affected files are held out of the apply, so nothing is lost while it
  is broken.

- `conflicts` on `sensor.gitops_agent_status`, counting the paths held
  back in both directions. Conflicts are in no plan, so they are absent
  from `pending_changes`, and without their own count the one thing that
  actually needs a person was invisible to automations.

### Changed

- `commit_back`'s automatic half no longer fires while
  `capture_live_changes` is on. Both would act on the same drift, one
  pushing a throwaway branch proposing exactly what the other has already
  committed to the tracked branch. The "Commit Back" button is
  unaffected: parking a set for review on purpose is still worth having.

## [0.5.0] - 2026-08-06

The first version. Nothing shipped before it, so everything the add-on does
is described here once, in the state it is in - there is no earlier release
for anything to have changed or been fixed against.

### Added

- File sync between a git repository and `/homeassistant`. The agent
  fetches the tracked branch every `interval_minutes`, compares the files
  the repository tracks against the live config, and reports the
  difference as a plan. With `dry_run` on - the default, and where a fresh
  install starts - that is all it does. Turn it off and applying copies
  the planned files in, asks Home Assistant to check the configuration,
  and then reloads or restarts it depending on `apply_after_pull`. Before
  any of that it takes a Supervisor backup, and every file it is about to
  overwrite is copied under `/data/backup/` first, so a bad commit is one
  Rollback away. It only ever touches paths the repository already tracks:
  a file that exists only live is left alone.

  What Home Assistant writes for itself is never synced in either
  direction - `.storage/`, databases and backups, Python bytecode
  (`__pycache__/`, `*.pyc`, `*.pyo`), `.HA_VERSION`, `.uuid`,
  `.ha_run.lock`, `.cache/`, `ip_bans.yaml`, `known_devices.yaml` and its
  `.bak`, ESPHome's `.esphome/` build cache, the Device Builder add-on's
  `.device-builder*` state, and `/config/image`, Home Assistant's
  uploaded-image store. `image` is matched only at the config root, so a
  `www/image/` folder of your own keeps syncing. A repository tracking a
  plaintext `secrets.yaml` or `secrets.yml` stops the cycle with a
  refusal naming the file rather than writing it live.

- The ingress dashboard, on the add-on's own page and optionally in the
  sidebar: sync state, the pending plan and its diff, a run history, an
  activity feed, and buttons for reconcile, apply, roll back, commit
  drift back, and import. Supervisor's ingress proxy is its only route
  in.

  The run history is the durable half of those last two. Every reconcile,
  apply, rollback and import that actually runs is appended to
  `/data/history.jsonl` with its start time, duration, commit, outcome
  and counts, and read back at startup - so a config that changed
  overnight can still be traced to the run that changed it, which the
  activity feed cannot do once the add-on has restarted. An apply that
  landed its files and then failed a registry layer is recorded as
  `partial` rather than as an error, because those files are live.
  Refusals are not recorded: they are not runs, and a dry-run refusal
  fires on every interval. The newest 200 runs are kept.

  Every button answers immediately and runs its operation in the
  background, so an apply that takes twenty minutes shows as "applying"
  from the moment it is pressed instead of hanging a request that the
  server would eventually cut off. The page then polls for changes and
  redraws itself when there are any, which also means a reconcile that
  ran on the interval, or through the webhook, appears without a manual
  refresh. A request that fails raises a dismissible banner rather than
  leaving the page silently stale, and an action the agent refuses - an
  import with `allow_import` off, an apply with no usable plan - is
  written to the activity feed instead of being dropped.

  Answering immediately means the response cannot carry an outcome, which
  a browser does not need and a script does. Every accepted POST returns
  an `X-GitOps-Op-Id` header, and `GET /status.json` carries a matching
  `operation` object with that id, whether it is still running, and what
  it returned - so a script can poll for the result of the operation it
  started rather than guessing from `busy`, which is false both before an
  operation starts and after it ends. A POST refused because something
  else is running returns `X-GitOps-Op-Refused: busy` and starts nothing.

  Standing health warnings appear as chips for as long as they last,
  rather than as a single line that scrolls out of the feed: history
  writes failing, add-on version records failing, an import that was
  pushed but whose record could not be saved, HACS missing while its
  layer is on, and per-slug update-check failures.

  The diff and activity panes are focusable, so they can be scrolled from
  the keyboard, and state changes are announced through a live region for
  screen readers. The stylesheet is served as a static file behind a
  content-hashed URL and cached for a year, while the page itself is
  never cached, so an update is picked up immediately and the CSS is
  fetched once. The canned preview states used for local UI work compile
  only under a build tag, so the shipped binary carries none of that
  invented text.

- Registry reconciliation, under `reconcile.registries`: sync Home
  Assistant floors, areas, labels, and helper entities (input_boolean,
  input_number, input_select, input_text, input_datetime, counter, timer)
  from `gitops/registries.yaml` and `gitops/helpers.yaml` in the config
  repository. Adopts existing objects by exact name match,
  creates/updates/deletes only what it manages, and rolls back registry
  changes alongside file changes.

- Entity reconciliation, under the same option: customize existing
  entities' name, icon, area, labels, and disabled/hidden state from
  `gitops/entities.yaml`. Update-only - it never creates or deletes an
  entity, and removing one from the manifest restores exactly the fields
  it ever changed.

- Dashboard reconciliation, under `reconcile.dashboards`: sync Lovelace
  dashboards (metadata and view config) from `gitops/dashboards.yaml`,
  following the same create/adopt/update/delete-only-managed model as
  floors, areas, and labels. A dashboard's id is its `url_path` spelled
  the way Home Assistant spells it, hyphens included, so a dashboard made
  by hand in the UI can be adopted; `default` and `lovelace` are
  reserved.

- Add-on options reconciliation, under `reconcile.addon_options`:
  customize other installed add-ons' options from `gitops/addons.yaml`.
  Update-only, read-merge-write, and it refuses to ever manage its own
  options.

- Config-flow integration reconciliation, under `reconcile.integrations`:
  create, adopt, and delete config-entry integrations (Settings > Devices
  & services) from `gitops/integrations.yaml`. Bounded to plain
  form-based setup flows - see DOCS.md for what is out of scope.

- Subentry reconciliation, under `reconcile.subentries`: create, adopt
  and reconfigure the child configurations an integration hangs off one
  of its config entries (a calendar under Google Calendar, a widget
  under PushWard) from `gitops/subentries.yaml`. Unlike an integration, a
  subentry can be updated in place, so editing its declared data applies
  live on the next reconcile; fields the manifest does not declare keep
  the values they have now. It never deletes: dropping an item from the
  manifest stops managing it and leaves the live subentry alone. Like
  integrations, drift is tracked by a fingerprint of the last applied
  data rather than by reading live state back - see DOCS.md for what
  that cannot see, and why there is no rollback.

- `commit_back` option: when a reconcile finds live drift while `dry_run`
  is on, push the live version of the drifted files to a new
  `gitops/drift-<timestamp>` branch instead of only ever discarding it on
  the next apply. Never touches the tracked branch. Also available as a
  "Commit Drift Back" button in the web UI. Once captured, the same drift
  set is not pushed again until it changes shape.

- `webhook_secret` option: when set, starts a small separate listener on
  port 8098 (declared but disabled by default) that triggers an immediate
  reconcile on a matching `POST /webhook`, gated by the secret via an
  `X-Gitops-Token` header or `?token=` parameter. The ingress dashboard on
  8099 is unaffected and still Supervisor-only.

- `allow_import` option (default `false`) plus "Preview Import" and
  "Import from Home Assistant" buttons: copies every non-excluded file
  under `/homeassistant` into the repository and pushes it as a single
  commit directly onto the tracked branch, so a repository can be seeded
  from a live install - something no other operation could do, since the
  diff only ever walks paths the repository already tracks. A repository
  with no commits at all is reported as its own state, `unseeded`, rather
  than as an error: the branch does not exist yet, so there is nothing to
  fetch and nothing to compare, and the dashboard says so and points at
  Import instead of showing a git failure. It repeats every interval
  without filling the activity feed or the run history, and an import (or
  a first commit pushed by hand) is what leaves it. A branch name that is
  merely mistyped reads the same way, which the banner says; credentials
  that do not work and a host that cannot be reached are still errors. The one
  operation in this add-on that writes to the tracked branch: manual
  only, never on the interval, fast-forward only, never forced, and it
  never deletes anything from the repository or claims ownership of what
  it captured. Secrets, databases, backups, `.storage/` and symlinks are
  skipped; an oversized live tree fails loudly, naming what blew the
  budget, before any git command runs.

  An import honors `.gitignore` before it reads, encrypts or copies
  anything, rather than letting `git add` drop those paths at the end
  after the work has been done. The config's own `.gitignore` files count,
  at any depth - ESPHome ships one excluding `src/`, `lib/`, `.pioenvs/`
  and `platformio.ini`, which was 898 generated C++ files on the install
  this was measured on - and where a file exists both live and in the
  repository, the live copy's rules win, because the import is about to
  commit it over the repository's anyway.

  A repository with no `.gitignore` at all gets one seeded, listing what
  HACS owns (`custom_components/`, `www/community/`) plus zigbee2mqtt's
  runtime state, Node-RED's credential store and per-instance settings,
  AppDaemon's compiled dashboards, `*.bak` and `.DS_Store`. On the install
  this was built against, HACS alone was 5113 of 5545 tracked files and 95
  MB of the 136 MB, and roughly 190 vendor translation files were being
  encrypted for holding a key called `integration_key` - no secret
  anywhere in them, at a `sops` call each per reconcile. HACS also updates
  that tree in place, so tracking it means an apply can roll an
  integration update back to whatever was committed. The file is written
  once, into a repository that has none, and never touched again: delete a
  line and import again to start managing something in it.

  Preview shows exactly what would land without touching git at all, and
  counts what would be committed rather than what was scanned - on a real
  config the scan finds 5860 files and the import commits 191. The
  gitignored total is shown alongside the other skip reasons.

- `age_key` option: secret values are encrypted with SOPS and an age key
  before they reach git, and decrypted again when applying, so a config
  repository can hold `secrets.yaml` without holding it in the clear.
  Encryption is per value, not per file - only the values behind
  secret-shaped keys (`password`, `mqtt_password`, `client_secret`,
  `network_key`, and the rest of the pattern documented in DOCS.md)
  become ciphertext, so a config file stays readable in a pull request.
  `secrets.yaml` is the exception and is encrypted whole, because every
  value in it is a secret; it also stops being an excluded path once a
  key is set, and syncs in both directions like any other file. With no
  key configured, secrets stay out of the repository entirely.

  The agent maintains a `.sops.yaml` at the repository root so that
  `sops secrets.yaml` in your own clone gets the same rules the agent
  applies; the agent's own `sops` calls never consult it, or any other
  `.sops.yaml` in the repository, so a rule added there cannot weaken
  what gets encrypted. Diffs on the dashboard, on `GET /status.json`
  and on `sensor.gitops_agent_status` have their secret values masked
  before they are published.

  Encryption covers YAML, JSON and dotenv files. JSON matters as much
  as YAML in a real config - a Google service account key is a `.json`
  whose `private_key` field already matched the secret-key rule, and a
  Zigbee coordinator backup is a `.json` full of network keys. dotenv
  covers the extensionless `KEY=value` files wmbusmeters keeps under
  `wmbusmeters.d/`, which hold a wM-Bus AES key on a `key=` line.

  A dotenv file is recognized only when it has no extension, every
  non-blank non-comment line is an assignment, and one of those keys is
  secret-shaped. The narrowness is deliberate: SOPS picks how to read a
  file from its extension, and a file it cannot place is encrypted
  whole into one opaque base64 blob with the per-value rule discarded,
  so a wrong guess costs the readable config file the whole feature
  exists to preserve. Files with an unrecognized extension are left
  alone rather than guessed at, and `.env` files stay refused outright
  as secret-shaped paths.

  A JSON or dotenv file's published diff collapses to "encrypted values
  changed (hidden)" rather than being masked line by line: the masking
  pass reads YAML, and pointing it at another grammar would mean
  deciding what is safe to publish by the wrong rules.

  Some files hold a secret that SOPS cannot encrypt without breaking
  something, and each is refused by name with the fix rather than
  encrypted anyway or committed in the clear: an inline secret next to
  a `!secret`, `!include` or `!input` tag (SOPS does not round-trip
  those tags); a top-level list, which `automations.yaml` always is; an
  unquoted `yes`/`no`/`on`/`off` in a file encrypted key-by-key, which
  SOPS would quote and Home Assistant would then read as a string; a
  literal top-level `sops:` key; and a file that does not parse as YAML
  but looks like it holds a secret. An empty or comment-only
  `secrets.yaml` is not an error - there is nothing in it to protect.
  Each format brings its own: a JSON file whose top level is not an
  object (SOPS only encrypts one starting with `{`), and a JSON or
  dotenv file that already uses SOPS's own metadata names - a top-level
  `"sops"` key, or any `sops_` key in a dotenv file, where a format
  that cannot nest forces SOPS to spread its metadata across the whole
  `sops_` prefix.

  An import reports every file it cannot encrypt in one error rather
  than stopping at the first, and stages nothing when there is any, so a
  partial snapshot never passes for a complete one. A missing or wrong
  key fails the cycle and applies nothing. A file in the repository that
  declares a master key other than age - a KMS or a Vault address,
  either of which would send `sops` off to a host the repository chose -
  is refused before `sops` runs. Losing the key means losing the
  encrypted values: keep a copy outside Home Assistant.

- `sops` is part of the add-on image. When `age_key` is set the agent
  runs `sops --version` at startup and refuses to start if the binary is
  missing or unusable, rather than finding out halfway through an import.

- `auto_update_addons` option: list an add-on's slug and the agent keeps
  that add-on updated for you, checking a couple of minutes after start
  and then every `auto_update_interval_minutes` - six hours by default,
  configurable from 15 minutes to 7 days. That cadence is its own: it
  never triggers a reconcile, and `interval_minutes` never triggers a
  check. Empty by default, which means no check runs at
  all. Each install is preceded by a partial Supervisor backup of just
  that add-on, so a bad release can be restored from Settings > System >
  Backups - an update is the one change Rollback cannot undo, because
  there is no downgrade call for the agent to make.

  It only ever updates add-ons that are already installed and named in
  the option. Nothing is installed from scratch, nothing is removed,
  Supervisor's own per-add-on auto-update toggle is left exactly as you
  set it, and the agent refuses to update itself whatever the list says:
  Supervisor would stop the container mid-call, leaving nothing to
  record how it went.

  With `dry_run` on the check is report-only. A failed check or install
  is shown on the dashboard and in the activity feed, but never sets the
  sync state to "error" - an add-on version is not in the repository, so
  a failed image pull says nothing about whether the config matches it,
  and the next check tries again. A check that cannot reach Supervisor is
  logged when it starts failing and when it recovers, rather than once
  per check for as long as the outage lasts. The "Add-on updates"
  card carries one row per configured slug - including a slug that is not
  installed, which is what a typo looks like - and counts the updates
  waiting, not the add-ons watched. `sensor.gitops_agent_status` carries
  `addon_updates_available` and `last_addon_update`.

- `track_addon_versions` option: record which add-ons are installed, and
  what version each is on, in `gitops/addon-versions.yaml` on the tracked
  branch. A config repository says what Home Assistant should look like
  but not which version of ESPHome produced the firmware in it, and after
  a rebuild that is the thing nobody wrote down.

  It records what it observes, whoever moved the version - a manual
  update from the UI, Supervisor's own auto-update, `auto_update_addons`,
  a restored backup - and it records rather than manages: nothing in the
  file is ever installed from it, and the agent never reads it back. The
  comparison that decides whether to rewrite it is byte-exact, so any
  hand edit at all - a comment, a reordering - is reverted on the next
  cycle. Like the rest of `gitops/` it is never copied into
  `/homeassistant` and never shows up as drift.

  Written at the end of a reconcile cycle that ran to completion, so a
  change is normally recorded within one `interval_minutes` - a cycle
  that stopped early records nothing until one completes. Almost every
  cycle writes nothing: the file is rendered deterministically and
  compared against what is already committed, so an install where nothing
  has been updated produces no commits however often the agent runs. The
  commit carries that one path and nothing else, whatever else happens to
  be staged in the agent's own checkout. The push goes onto the tracked
  branch fast-forward only and is never forced - a concurrent push costs
  one retry on the new tip, not somebody's work. `dry_run`
  does not gate it, since it writes to the repository rather than to Home
  Assistant, but `git_token` does need push rights. Each committed record
  names the versions that moved in the activity feed, collapsed to a
  count past the first five, and a failure to record is a warning there
  rather than a sync error.
