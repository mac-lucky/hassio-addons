# Changelog

All notable changes to this add-on are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

Nothing yet.

## [0.8.4] - 2026-10-08

Rebuild only, no functional change.

### Changed

- Base image updated to hassio-addons/base 21.0.8 (from 21.0.7).

### Security

- Picks up Alpine's zlib 1.3.2-r1 (CVE-2026-85091), published after 0.8.3
  was built.

## [0.8.3] - 2026-10-06

Rebuild only, no functional change.

### Changed

- Base image updated to hassio-addons/base 21.0.7 (from 21.0.6).

### Security

- Picks up Alpine's pcre2 10.49-r0 (CVE-2026-103111), published after 0.8.2
  was built.

## [0.8.2] - 2026-09-29

### Changed

- The left-behind check reads only the files that changed since the last
  applied and the last imported commit, instead of listing both full
  trees on every sync. The warning it raises is unchanged.

## [0.8.1] - 2026-09-28

### Added

- A warning in the activity feed and the add-on log when a commit removes
  or renames a file the agent never wrote, so the old copy stays live:
  "left in place: N file(s) removed from the repository stay live because
  this agent never wrote them - delete by hand if unwanted: <paths>".
  Deletion scope is unchanged - the agent still only deletes files it
  applied itself - but a rename of an imported file no longer leaves the
  old one running without a word. Files are found against both the last
  applied and the last imported commit. Each set of such files is
  reported once (an add-on restart can report it again); the feed names
  at most 20 paths and the add-on log has the full list.

## [0.8.0] - 2026-09-28

Three new things to manage from `gitops/` - device settings, zones and
persons, and Lovelace resources - and a sync engine that copes better
with a forge that goes away for a few minutes. No new options. Two
behaviour changes to know about: a failed registry layer no longer stops
unrelated ones (see Changed), and with `reconcile.dashboards` on, anyone
who can push to the repository can now also add Lovelace resources,
which run in every user's browser (see Added).

### Added

- Zones and persons in `gitops/helpers.yaml`, under `zone:` and `person:`,
  created, adopted, updated and deleted like the other helpers. A zone
  needs `name`, `latitude` and `longitude`; a person takes `name` and
  `device_trackers`, while its linked user and picture stay whatever the
  UI sets, since neither carries over between installs. Home Assistant
  merges updates to both, so `null` is refused on their fields. The home
  zone and YAML-defined zones and persons cannot be managed; a declared
  person named like a YAML one is reported instead of created.
- `gitops/devices.yaml`: the name, area, labels and disabled state of
  devices that already exist, under `reconcile.registries`. Each entry
  finds its device by the integration's name for it, an identifier or a
  device id, and must match exactly one. Update-only, like entities:
  removing an entry puts back the values it changed, and nothing is
  restored while an entry that sets anything matches no device (a renamed
  device should not lose its settings in the meantime). `disabled` is refused on a
  device an integration disabled.
- Lovelace resources in `gitops/dashboards.yaml`, under `resources:`,
  with the dashboards toggle. A resource is identified by its URL path, so
  HACS's `?hacstag=` rewrites are not drift and declaring a HACS card
  adopts its existing entry. Undeclared resources are never touched. Not
  available when Home Assistant keeps resources in YAML mode. A resource
  is script that runs in every user's browser session, so with
  `reconcile.dashboards` on, push access to the repository now also
  means that (see DOCS.md, "Lovelace resources").
- `fetch_failing_since` on `sensor.gitops_agent_status`, and a "Git host
  unreachable" notice on the dashboard (see Changed).
- `pending_device_ops` and `pending_resource_ops` on
  `sensor.gitops_agent_status`, and "devices (by device id)" and
  "Lovelace resources" groups on the dashboard's "Managed by this agent"
  card.

### Changed

- A git host that stays unreachable past the two quick fetch retries no
  longer puts the add-on in the error state straight away. For up to 15
  minutes the state, the pending plan and the last error stay as the last
  successful check left them, nothing is applied automatically, and one
  warning is logged; after that it is an error as before. A self-hosted
  forge stopped for a few minutes by its nightly backup used to turn the
  add-on red every morning. A rejected token or a missing repository is
  still an error at once, and so is an unreachable host right after the
  add-on starts.
- A registry layer that fails to apply no longer stops every layer after
  it. Each layer's changes were planned against live state before the
  apply, so a failed dashboard save now leaves add-on options, HACS and
  integrations to apply as planned. Only real dependencies still wait:
  devices and entities on floors/areas/labels/helpers, entities on
  devices, integrations on HACS, and subentries on every other layer (a
  subentry nothing recorded would come back as a duplicate). The
  activity log names every layer that failed and every layer held back,
  and `last_error` names each failed layer when more than one did.
- Webhook deliveries are queued instead of dropped. A push announced
  while a cycle (or Apply, or Roll Back) was running found the agent busy
  and was lost until the next interval; it now queues another cycle, and
  every delivery arriving in the meantime shares at most two; only one queued
  behind a Roll Back is dropped, since the rollback pauses until the
  repository is fixed. Webhook cycles run at least 10 seconds apart.
- The webhook answers a `ping` with `200 pong`, and ignores (with `200`) a
  push to another branch or tag and any event other than a push, instead
  of running a cycle for each. A request that names no event, such as a
  script's, still always triggers.
- A signed webhook body identical to one accepted in the last hour is
  answered `200 duplicate delivery ignored` and runs nothing, so a
  captured delivery cannot be replayed to keep the agent busy. Token
  requests are never treated as duplicates.

### Fixed

- A diff hidden because a plaintext file holds secret values (for example
  a JSON or dotenv file with a `password` key) said `encrypted values
  changed (hidden)`, sending people to look for sops or an age key that
  had nothing to do with it. It now says `diff hidden: the file holds
  secret values`, as does the diff of any file being deleted, which
  quotes the plaintext live copy; the encrypted wording stays for adds
  and updates of encrypted files.

## [0.7.0] - 2026-09-28

Fixes from a review of the sync engine and the registry layers, plus
signed webhook deliveries. No new options, but three behaviour changes
to know about (see Changed): with `dry_run` off a webhook delivery now
applies, a successful Roll Back pauses automatic checks, and
`gitops/helpers.yaml` is checked more strictly.

### Added

- The webhook trigger accepts a git host's signed delivery. Put the
  value of `webhook_secret` in the webhook's secret field on GitHub,
  Forgejo or Gitea and drop `?token=` from the URL: the agent checks the
  HMAC-SHA256 of the body in `X-Hub-Signature-256`, `X-Forgejo-Signature`
  or `X-Gitea-Signature` against the same secret, so the secret itself
  never crosses the network. Port 8098 is plain HTTP, so this is the
  better option whenever the host reaches the agent over a network. The
  `X-Gitops-Token` header and `?token=` keep working unchanged; a
  request carrying a token is judged on the token alone. Bad signatures
  count toward the same 30-a-minute lockout as wrong tokens, and a
  signed body over 5 MiB is answered `413` without triggering anything.
- Fetches retry twice (after 2 and 8 seconds) when the failure looks
  transient: an HTTP 5xx from the forge, a DNS failure, a refused or reset
  connection. A forge that is back within a few seconds, such as one
  restarting behind its proxy, no longer puts the agent in the error
  state.
- `apply_held` on `sensor.gitops_agent_status`, and an "Automatic apply
  held" notice on the dashboard (see Fixed).

### Changed

- A webhook delivery now runs a full cycle, apply included, instead of
  only checking. With `dry_run` off a push lands as soon as the forge
  announces it rather than up to an interval later. While paused, or with
  `dry_run` on, it still only checks.
- Activity events now reach the add-on log at their real level: failures
  as `ERROR`, degraded outcomes (backup failed, conflict, config warnings)
  as `WARN`. Every event used to be logged as `INFO`, so a log shipper
  filtering on level never saw a failed cycle.
- Pressing Apply applies only the plan the page showed. If a webhook or the
  timer replaced it in between, nothing is applied and the dashboard says
  so; review the new plan and press again. Scripts that post to `/apply`
  without a `plan` value keep the old behaviour.
- `/status.json`'s `operation.error` now reports a failed or refused Apply
  or Roll Back. It was empty for both, whatever happened.
- After a successful Roll Back with `dry_run` off (or `capture_live_changes`
  on), automatic checks are paused. Nothing about the repository changed,
  so the next check used to re-apply the commit just rolled back (or
  capture the restored files over it). Resume once the repository is fixed.
- Clone and fetch get 10 minutes instead of 60 seconds, so a first clone
  of a repository with media in it can finish; a transfer that stalls
  (under 1 KiB/s for 30 seconds) is aborted and retried instead of
  holding the agent for the whole budget.
- `gitops/helpers.yaml` now refuses an on/off field written as anything
  but `true` or `false`. `initial` on `input_boolean`, `has_date` and
  `has_time` on `input_datetime`, and `restore` on `counter` and `timer`
  written as an unquoted `on`, `yes`, `off` or `no` is read as text,
  which Home Assistant stores as a boolean, so the item never matched and
  was re-applied on every cycle. Such a file now fails to load with an
  error naming the item and field; write `true` or `false` instead.
- Adopting a floor, area or label by name now matches the way Home
  Assistant compares their names, ignoring case and spaces. A manifest
  `living room` against a live `Living Room` used to plan a create that
  Home Assistant refused as a duplicate on every cycle; it now adopts the
  existing one and renames it to the manifest's spelling. Helpers still
  match exactly, since Home Assistant does not require their names to be
  unique.
- Encrypted YAML, JSON and dotenv files are now written with sops'
  `--mac-only-encrypted`, and the managed `.sops.yaml` sets
  `mac_only_encrypted: true` on its YAML and JSON rules. The ordinary
  values in such a file can now be edited by hand in a pull request
  without breaking decryption. Existing files keep their old whole-file
  check until they are next encrypted and still decrypt as before; the
  `.sops.yaml` in the repository is rewritten once, with the next import,
  capture or commit-back. The bundled sops already qualifies; editing
  these files by hand now needs sops 3.9.0 or newer.
- Base image updated to hassio-addons/base 21.0.6 (from 21.0.5).

### Fixed

- A commit that fails `check_config` is no longer re-applied every
  interval. Each attempt took a full Supervisor backup, recorder database
  included, and backups from failed applies were never pruned, so a bad
  commit could fill the disk. The timer now holds a plan whose apply
  failed and tries it again after an hour, then two, four and so on up to
  a day; a different plan (a new commit, a live edit) is not held, and the
  Apply button still tries it at once. Backups are pruned after every
  apply, failed or not.
- A failed apply that undid itself no longer moves the Roll Back target
  onto its own, already restored, stash. Roll Back keeps pointing at the
  last good apply, and that stash is no longer aged out by repeated
  failures.
- `reconcile.yaml_files: false` now actually stops file sync. It was read
  and ignored: every tracked file was still written into `/homeassistant`.
- A conflict (a file changed both in the repository and live) stays a
  conflict until the two sides agree. An apply of other files in the same
  cycle used to clear it, and the next cycle captured the live copy over
  the repository's commit.
- `capture_live_changes` no longer reverts a push that lands on a captured
  path between the check and the capture. Such a path is left out of the
  capture and comes back as a conflict on the next cycle.
- Stopping the add-on during a capture or commit-back no longer leaves the
  local clone on the capture branch, where the apply that follows would
  write a tree nobody reviewed. An apply now also checks out the commit it
  planned from if the clone moved, and no new apply starts once shutdown
  has begun.
- Pausing now stops an automatic add-on update batch between add-ons; it
  used to be checked once, before the batch.
- A clone that fails or is interrupted no longer leaves a half-built
  repository behind that later cycles treat as complete.
- Renaming a floor, area, label or helper `id` in the manifest while
  keeping its `name` no longer fails on every cycle. The old id still
  held the live object, so the new id could not adopt it and its create
  collided with the existing name - and a helper, which Home Assistant
  lets share a name, came back as a second `<name>_2` entity while the
  original was deleted. The new id now adopts the object and the old id
  is forgotten in the same apply; nothing is deleted or recreated.
- Renaming an integration `id` the same way no longer deletes the
  integration. The new id's setup flow aborted as already configured and
  was recorded as a failure, while the old id's delete removed the
  original entry along with its devices and entities. The new id now
  adopts the entry and the old id is forgotten.
- A managed floor, area, label, helper or dashboard that was deleted by
  hand and then removed from the manifest is now forgotten instead of
  tracked forever. Their live ids come from their names (or url_path),
  so an object created later under the same name was deleted by the next
  apply as one the agent still managed. The plan shows a `forget` line
  for it, and nothing is sent to Home Assistant.
- A rename whose new id cannot adopt yet - an ambiguous name match, an
  area with a broken floor or label reference, an integration with an
  unresolvable `secret://` reference - no longer deletes the old id's
  object. It is left alone until the error is fixed.
- Updating a helper no longer strips the fields the manifest does not
  declare. Home Assistant replaces a helper's whole configuration on
  update, so adopting an `input_number` by name alone dropped its icon,
  unit and mode, and failed outright without `min` and `max`. The agent
  now sends the undeclared fields back as they are live; a field
  declared `null` is removed, except one Home Assistant fills with a
  default (such as `restore` or `step`), where `null` is refused at load
  because it would be re-applied every cycle. Rolling back a helper update now restores
  the whole prior configuration instead of sending `null` for fields
  that were unset before, which Home Assistant rejected.
- An area declared with `floor: null` now clears its floor. It used to
  be planned as a reference to a floor with an empty id, which could
  never resolve, failing the whole registry layer on every cycle.
- A field declared `null` is left out of a create rather than sent:
  Home Assistant's create schemas reject `null` for most fields.
- A registry manifest no longer fails every cycle on an install without
  `default_config:`. The agent used to list every helper domain it
  supports, and a helper integration that is not loaded answers with
  `unknown_command`; it now lists only the domains the manifest declares
  or it already manages, and names a used-but-unloaded one in the error.

### Security

- A secret typed into a plaintext-tracked file (for example an `api_key:`
  added in the File editor) is now masked in the dashboard diff and in
  `/status.json`. Update diffs were masked only when the repository copy
  was encrypted.

## [0.6.11] - 2026-09-25

Rebuild only, no functional change.

### Changed

- Base image updated to hassio-addons/base 21.0.5 (from 17.2.5).

## [0.6.10] - 2026-09-19

Rebuild only, no functional change.

### Security

- Picks up Alpine's OpenSSL 3.3.7-r1 (CVE-2026-63073, CVE-2026-34182,
  CVE-2026-75803 and six High-severity fixes), published after 0.6.9 was
  built.

## [0.6.9] - 2026-09-07

Rebuild only, no functional change.

### Security

- OpenSSL and curl are now actually upgraded. The base image pins the
  versions it shipped with in `/etc/apk/world`, so `apk upgrade` had been
  skipping them; the pins are dropped before the upgrade.

## [0.6.8] - 2026-09-06

Rebuild only, no functional change.

### Security

- The release build no longer reuses cached image layers. 0.6.7 was built
  from a cached layer and still shipped the base image's OpenSSL 3.3.3
  instead of the patched 3.3.7 that `apk upgrade` fetches on a fresh build.

## [0.6.7] - 2026-09-06

Rebuild only, no functional change.

### Security

- Built with Go 1.27.1 and golang.org/x/crypto 0.56.0. The 0.6.6 image was
  built with an older toolchain and carried fixable standard-library and
  x/crypto vulnerabilities; the Alpine packages are refreshed as well.

## [0.6.6] - 2026-09-02

Hardening follow-ups from a fresh review of 0.6.5. No new options.

### Fixed

- Entity ownership is now re-checked at the moment an entity op is applied,
  not only when the plan was built. Plans are applied later than they are
  planned - the whole time a dry-run diff sits on the dashboard waiting for
  Apply - and an integration that claimed an entity's `disabled_by` or
  `hidden_by` in between was silently overwritten, with its actual state
  lost from the restore record. Such an op is now refused and surfaces as
  an error; the next cycle re-plans around it.
- A timed-out git operation no longer leaves its transport helper
  (`git-remote-https`, `ssh`) running as an orphan holding a connection.
  Subprocesses now run in their own process group and the whole group is
  killed on timeout.
- Files written into /homeassistant are now written atomically (temp file,
  fsync, rename), so a power cut mid-write cannot leave one half-written.
  The pre-write backup already made this recoverable; now it does not
  happen in the first place.

### Security

- Version strings and status lines on the add-on updates card are now
  escaped like every other external text on the dashboard. An add-on
  repository's published version or a Supervisor error message could carry
  invisible Unicode direction-override characters and misrender what the
  card shows.
- A file carrying spoofed sops metadata (a `sops:` block with a mac and
  version but no key source at all) is now classified as what it is:
  plaintext. Nothing could ever decrypt such a file; treating it as
  encrypted meant a secrets-shaped file with a spoofed block sailed past
  the tracked-in-clear guard and failed later with a confusing decrypt
  error instead of an honest refusal.

## [0.6.5] - 2026-09-02

A follow-up security release: an adversarial review of 0.6.4 turned up
gaps in the sync engine's trust boundaries, all closed here. No new
options; one new file appears under /data (see below).

### Added

- `git_token`, `age_key` and `webhook_secret` can now hold a
  `secret://<name>` reference instead of the literal value, resolved from
  the live secrets.yaml once at startup. Add-on options are readable in
  the clear over the Supervisor API (`format: password` only masks the
  UI), so anything with hassio-API access could read the forge token and
  the age private key straight out of them; a reference keeps only the
  pointer there. Literal values keep working unchanged; a reference that
  does not resolve stops the add-on instead of being used literally.

### Security

- The refusal to decrypt sops documents naming a remote key backend (kms,
  vault and friends) could be bypassed. The check walked the YAML document
  literally, but sops resolves aliases, merge keys and tagged keys when it
  reads the same metadata - so a repository could smuggle a `hc_vault`
  master key past the check and make the agent's decrypt call reach out to
  a repository-chosen URL on every cycle, dry run included. The check now
  reads the metadata the way sops does and refuses anything it cannot.
- Rolling back now re-checks every path in the backup manifest the way the
  apply did: absolute paths, `..`, and parent directories swapped for
  symlinks since the apply are refused instead of followed, so a rollback
  can no longer be redirected to write or delete outside /homeassistant.
  Exclusion patterns are deliberately not re-checked - a stash records
  what the apply actually touched, so clearing `age_key` between an apply
  and its rollback no longer strands the restore.
- The fingerprints the agent stores in state.json for declared
  integrations, subentries and HACS entries are now keyed (HMAC) with a
  random key kept in the new `/data/failmemory.key`. They were plain
  hashes of the resolved data - secret values included - so anyone holding
  a shared state.json could test password guesses against them offline.
  Fingerprints written by earlier versions are still recognised, so
  nothing re-plans or re-runs after the update.
- The dashboard now makes invisible Unicode formatting characters visible
  (as `\u{...}` escapes) in diffs, paths, error text and the activity
  feed. A repository commit could otherwise use bidirectional override
  characters to make the pending-changes diff read differently from the
  bytes an Apply would write - the Trojan Source trick - defeating the
  review the dry-run gate exists for.

### Changed

- CI now verifies the gitleaks release tarball against a pinned sha256
  before running it; it was the one executed artifact a forge-side swap
  could have replaced silently.

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
