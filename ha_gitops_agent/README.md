# GitOps Agent

A Home Assistant app that keeps your `/config` in sync with a git
repository: dry-run diff, validate-then-apply, and rollback on
failure. Optionally also reconciles, from a `gitops/` directory in the
same repository: floors, areas, labels, and helper entities; entity
customizations; Lovelace dashboards; other apps' options; and
config-flow integrations. Can also sync live edits back to the tracked
branch (`capture_live_changes`) or to a review branch (`commit_back`),
seed a fresh repository from an existing config (`allow_import`), and be
triggered on demand over a secret-gated webhook, instead of only ever
polling on an interval.

With an age key configured (`age_key`), secret values are encrypted with
SOPS before they reach git and decrypted again on apply, so `secrets.yaml`
can live in the repository without living there in the clear.

## Install

1. Add this repository to your Home Assistant app store:
   `https://github.com/mac-lucky/hassio-addons`. For local
   development, copy `ha_gitops_agent/` to `/addons/ha_gitops_agent/`
   on the host instead, then go to **Settings > Add-ons > Add-on
   Store**, open the three-dot menu, and choose **Check for updates**
   (or **Reload**) - the add-on then appears under **Local add-ons**.
2. Install "GitOps Agent" from the store.
3. Open the add-on's Configuration tab and fill in the options form
   (repository URL, branch, credentials, sync interval), then start
   the add-on. A fresh install starts with `dry_run` enabled and no
   repository configured, so it will not write anything until you
   fill in the options and turn `dry_run` off.
4. Open the add-on's page and turn on **Show in sidebar** to get the
   GitOps dashboard in the Home Assistant sidebar. Until then it is
   reachable via **Settings > Add-ons > GitOps Agent > Open Web UI**.
   Use it to trigger a manual reconcile, review the diff, and apply.

See [DOCS.md](DOCS.md) for the full option reference and safety model.

## Development

The agent is a single Go binary; `go.mod` lives in this directory,
alongside the rest of the add-on. From here:

```sh
go build ./...
go test ./...                        # main test lane
go test -tags dev ./internal/web/    # web preview fixtures, dev tag only
go vet ./... && gofmt -l .
golangci-lint run                    # formatters configured in .golangci.yml
```

`GITOPS_DEV=1 go run -tags dev ./cmd/ha-gitops-agent` serves the dashboard on
`http://localhost:8099` with canned preview states, so you can work on the UI
without a real repository or a real Home Assistant behind it.

The add-on image builds with a multi-stage `Dockerfile`, so installing it
needs no local Go toolchain at all.

### Testing a build on a real box

`config.yaml` sets `image:`, so Supervisor pulls the prebuilt image from
GHCR rather than building on the box - which is what you want for an
install, and a trap for local development. Copying the source into
`/addons` and pressing Rebuild still builds `local/<arch>-addon-...`, but
Supervisor then tries to start `ghcr.io/...` and the add-on fails with
`Image ... does not exist`.

Two ways round it, on the box:

```sh
# Test what CI published (what a user would install)
docker pull ghcr.io/mac-lucky/hassio-addon-ha-gitops-agent-amd64:<version>

# Test an uncommitted local build: rebuild, then give it the name
# Supervisor is going to look for
docker tag local/amd64-addon-ha_gitops_agent:<version> \
  ghcr.io/mac-lucky/hassio-addon-ha-gitops-agent-amd64:<version>
```
