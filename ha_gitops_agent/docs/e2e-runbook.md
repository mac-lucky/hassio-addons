# Live end-to-end runbook

Unit tests cover every layer against fakes; they cannot catch AppArmor
denials, Supervisor API quirks, or Home Assistant schema drift. Those only
show up on a real box. This is the checklist for running the full add-on
against real hardware without leaving a mark on it.

Time: a full pass is a day. A single-layer smoke check is under an hour.

## Ground rules

- Take a full Supervisor backup by hand before touching anything. The
  add-on's pre-apply backup is partial and is not the safety net here.
- Every object the run creates is named `gitops-e2e-...`. Nothing
  pre-existing is ever named in a manifest, except an adopt test adopting
  an object this run created moments earlier.
- `reconcile.integrations` deletes config entries, which permanently
  deletes their devices and entities. Declare only throwaway core domains
  (`moon`, `local_ip`, `time_date`, `workday`). Never a domain with real
  devices behind it.
- `reconcile.addon_options` restarts the add-on it edits. Target one
  harmless option on one non-critical add-on.
- Enable layers one at a time, `dry_run: true` first, read the diff in the
  UI, then apply. Never flip everything on at once.
- Keep a running findings log as you go. A failed phase must be
  reproducible from the log, not from memory.

## Test bench access

- The add-on's web UI only answers the ingress proxy (172.30.32.2). To
  curl it, exec inside the Supervisor container:
  `sudo docker exec hassio_supervisor curl -s http://<addon-ip>:8099/status.json`
- The Supervisor API needs the add-on's own token:
  `sudo docker exec <addon-container> printenv SUPERVISOR_TOKEN`.
  The token rotates on every add-on restart; re-extract it each time.
- The webhook port (8098) has no source restriction; probe it
  container-direct. It only listens while `webhook_secret` is set.
- Deploying a local build: copy the add-on directory to the Supervisor
  `addons` share, then reload the store and rebuild. The `ha` CLI in the
  SSH add-on may lack API scope for both; POST `/store/reload` and
  `/addons/<slug>/rebuild` on the Supervisor API with the token above
  always work. A rebuild does not reload `apparmor.txt`; an AppArmor
  change needs uninstall + install, which wipes the options.
- Exec inside the add-on container is confined by AppArmor; `git` runs,
  most other binaries do not. Pipe git output to the host side for
  counting or filtering.

## Phases

1. Baseline. Full manual backup. Capture: repo tree + HEAD, `/homeassistant`
   listing, config entries, dashboards, floors/areas/labels, entity count,
   add-on options. The final phase diffs against this capture.
2. Files. Commit one `packages/gitops_e2e.yaml` template sensor. Check,
   apply, confirm the entity. Delete from the repo, apply, rollback,
   re-apply. This smoke-tests the whole apply path. The run history card
   must gain a row per operation with the right kind, commit and counts -
   and the rollback row must show no commit. Then restart the add-on and
   confirm those rows are still there while the activity feed has reset:
   that is the one assertion no unit test can make, and the whole point
   of the file. `jq -s . < /data/history.jsonl` must parse.
3. Registries. Create a floor, area, label, and helpers (input_boolean,
   counter, timer). Adopt a hand-made area by name. Customize one entity
   (icon, area, labels) and confirm removal restores only those fields.
   Update a floor name, rollback, confirm HA accepts the inverse. Remove
   one object from the manifest and confirm delete-only-managed.
4. Dashboards. Create one with a view listing the phase 3 helpers. Confirm
   sidebar + render. Add an installed HACS card to the view. Change only
   the title, then only the view config, and confirm they diff as separate
   ops. Declare `id: default` and confirm the manifest-load error. Delete.
5. HACS artifacts. Install a small frontend card; its files appear as
   drift. Turn on `commit_back`, confirm a `gitops/drift-<ts>` branch with
   exactly those files and `main` untouched. Merge, confirm `in_sync`.
   Uninstall, confirm drift in the other direction commits back too.
6. Integrations. Create with no data (`moon`), one field (`local_ip`),
   declared data (`time_date`). Adopt a hand-made entry by domain + exact
   live title. Trigger the no-declared-data error and confirm it lists the
   step's fields. Confirm failure memory (same data reports the remembered
   failure, changed data retries once), the no-update rule, and that
   delete only removes the managed entry. Rollback of a delete re-creates
   under a new entry_id - that is documented, verify the warning holds.
7. Add-on options. Declare one option, apply, confirm read-merge-write
   left the others alone. Declare the agent itself and confirm the
   refusal. Remove the declaration and confirm the recorded original is
   restored - including an option that was absent before, which must be
   removed, not set to null.
8. Import + webhook. Preview Import, compare its file count against
   `git ls-files` after an import; the difference is gitignored runtime
   state. Set `webhook_secret`, POST /webhook: good token 202 plus a
   reconcile, bad token 403, GET 405; unset it and confirm the port
   closes.
9. Secret encryption. Needs the 0.5.0 AppArmor profile, so uninstall +
   install first and re-enter every option. Generate a throwaway key
   (`age-keygen -o /tmp/e2e-age.txt`), put the `AGE-SECRET-KEY-1...` half
   in `age_key`, restart, and confirm the startup log names the derived
   `age1...` recipient and nothing else. Live side: a `secrets.yaml` with
   two values, plus one file mixing plain keys with a secret-shaped one
   (`packages/gitops_e2e_secret.yaml`). Import, then read the pushed
   blobs off the remote: `secrets.yaml` is sops-shaped with no plaintext
   value left; the mixed file still shows its plain keys in the clear
   with only the secret-shaped value as `ENC[...]`; `.sops.yaml` carries
   the recipient and never the identity. Re-import with nothing changed
   and confirm it makes no commit - sops ciphertext is nondeterministic,
   and this is the churn trap. Then both directions: `sops secrets.yaml`
   in a clone, change a value, push, apply, and confirm the plaintext
   lands live; change the same value live instead and confirm the
   `gitops/drift-<ts>` branch carries it re-encrypted rather than in the
   clear. Negative: swap `age_key` for a different valid key, restart,
   and confirm the cycle fails naming the file, `/homeassistant` is
   untouched, and the dashboard says why - then put the right key back.
   Throughout, `/status.json` and the dashboard diff must show `*****` or
   "encrypted values changed (hidden)" and never a secret value; grep the
   add-on log for the identity and for each secret value and expect
   nothing.
10. Add-on auto-update. Nothing here touches the repository, so it can run
    beside any other phase. Set `auto_update_addons` to three slugs - one
    installed add-on you do not mind restarting, one that does not exist,
    and this add-on's own - with `dry_run` still on, restart, and wait out
    the two-minute startup delay. The "Add-on updates" card must carry one
    row per slug (current or `current -> latest`, "not installed", and the
    self refusal), nothing may be installed, and the sync state must stay
    whatever it was. Then the role check, which no fake can answer: POST
    `/store/addons/<slug>/update` for an add-on that is already current
    must not come back 403. A 400 saying there is no update is the pass -
    it proves `hassio_role: manager` reaches the update route, which is
    the one permission this feature needs and the manifest cannot assert.
    Finally, with `dry_run` off and a real update waiting, confirm the
    install, the partial backup under Settings > System > Backups, and
    `addon_updates_available` / `last_addon_update` on the sensor.
11. Cleanup. Empty the manifests, apply, verify deletion in HA directly.
    Delete drift branches, clear `age_key` and delete the throwaway key
    file, clear `auto_update_addons`, restore add-on options, uninstall
    test HACS repos. Diff against the phase 1 baseline: anything that
    differs is a finding or an incomplete cleanup. Done means `in_sync`,
    0 pending, no errors.

## Findings discipline

Every bug found here gets a regression test in the repo before the fix
ships, marked with a `// --- VM e2e: ... ---` section comment naming what
the hardware showed. Fix, review, then re-verify on the box before moving
to the next phase.
