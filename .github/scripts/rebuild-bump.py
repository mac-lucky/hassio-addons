#!/usr/bin/env python3
"""Turn a rebuild request for some GHCR images into releases of the add-ons
they belong to. The Supervisor offers an update only for a new version, so a
rebuild under the same one would reach nobody.

Usage (from the repo root, tags fetched):
  rebuild-bump.py plan <image>...   one "<slug> <action>" line per add-on
  rebuild-bump.py bump <slug>       bump it; prints title=... and functional=...

An image belongs to an add-on when it is the add-on's config.yaml `image:`
with `{arch}` replaced by one of its `arch:` entries, without the registry
(hassio-addon-vector-amd64). `plan` exits 1 when no image belongs to one.

plan's actions:
  bump   config.yaml names the newest tag's version: bump the patch.
  tag    config.yaml names a newer version that was never tagged (its tag
         run failed or was cancelled): tag that one, its release builds fresh.
  skip   no tag yet (the first release is cut by hand), or config.yaml names
         a version older than the newest tag.

bump rewrites config.yaml `version:`, the Dockerfile's `ARG BUILD_VERSION=`
default where it has one, and adds a CHANGELOG entry. The entry takes over
whatever the add-on's `[Unreleased]` section holds; without one, it lists the
subjects of the commits that touched the add-on since its newest tag. Either
way that is more than a rebuild, and `functional=true` says so, except for
chore commits (Renovate's dependency bumps): a chore publishes no release of
its own, so it rides along with the rebuild and holds back no auto-merge.
The scan dispatches a rebuild only for a fixable high or critical finding,
so the title is a fix.
"""
import datetime as dt
import re
import subprocess
import sys
import textwrap
from pathlib import Path

REGISTRY = "ghcr.io/mac-lucky/"
VERSION_RE = re.compile(r"^(version:\s*[\"']?)(\d+\.\d+\.\d+)([\"']?\s*)$", re.M)
BUILD_VERSION_RE = re.compile(r"^ARG BUILD_VERSION=(\S+)$", re.M)
CHORE_RE = re.compile(r"^chore(\([^)]*\))?:")


def run(*args):
    return subprocess.run(args, check=True, capture_output=True, text=True).stdout


def addon_images(config):
    image = run("yq", "-r", ".image", str(config)).strip()
    if not image.startswith(REGISTRY):
        return set()
    arches = run("yq", "-r", ".arch[]", str(config)).split()
    return {image[len(REGISTRY):].replace("{arch}", arch) for arch in arches}


def version_key(version):
    return tuple(int(x) for x in version.split("."))


def newest_tag(slug):
    pattern = re.compile(rf"{re.escape(slug)}/v(\d+\.\d+\.\d+)")
    versions = [m.group(1) for m in map(pattern.fullmatch, run("git", "tag", "--list", f"{slug}/v*").split()) if m]
    return max(versions, key=version_key) if versions else None


def config_version(slug):
    m = VERSION_RE.search(Path(slug, "config.yaml").read_text())
    return m.group(2) if m else None


def changelog(text, new, since, date, subjects):
    """(new text, whether the entry holds more than the rebuild)."""
    security = textwrap.fill(f"- Rebuilt on the base image's current packages, picking up the fixes "
                             f"published after {since} was built.", width=80, subsequent_indent="  ")
    unreleased = re.search(r"^## \[Unreleased\][^\n]*\n(.*?)(?=^## \[|\Z)", text, re.M | re.S)
    pending = unreleased.group(1).strip() if unreleased else ""
    if pending == "Nothing yet.":
        pending = ""
    if pending:
        heading = re.search(r"^### Security[ \t]*\n\n?", pending, re.M)
        if heading:
            body = pending[:heading.end()] + security + "\n" + pending[heading.end():]
        else:
            body = f"{pending}\n\n### Security\n\n{security}"
    elif subjects:
        changed = "\n".join(f"- {s}" for s in subjects)
        body = f"### Changed\n\n{changed}\n\n### Security\n\n{security}"
    else:
        body = f"Rebuild only, no functional change.\n\n### Security\n\n{security}"
    section = f"## [{new}] - {date}\n\n{body}\n\n"
    functional = bool(pending or any(not CHORE_RE.match(s) for s in subjects))
    if unreleased:
        return text[:unreleased.start(1)] + "\nNothing yet.\n\n" + section + text[unreleased.end(1):], functional
    first = re.search(r"^## \[", text, re.M)
    if first:
        return text[:first.start()] + section + text[first.start():], functional
    return text.rstrip("\n") + "\n\n" + section.rstrip("\n") + "\n", functional


def plan(images):
    wanted = set(images)
    picked = []
    for config in sorted(Path(".").glob("*/config.yaml")):
        mine = addon_images(config)
        if mine & wanted:
            picked.append(config.parent.name)
        wanted -= mine
    for image in sorted(wanted):
        print(f"::notice::{image} is not an add-on image of this repository; skipped", file=sys.stderr)
    if not picked:
        print("::error::none of the images belongs to an add-on here", file=sys.stderr)
        return 1
    for slug in picked:
        version, newest = config_version(slug), newest_tag(slug)
        if not version:
            action = "skip"
            print(f"::warning::{slug}/config.yaml has no X.Y.Z version line", file=sys.stderr)
        elif not newest:
            action = "skip"
            print(f"::warning::{slug} has no {slug}/v* tag yet; cut its first release by hand", file=sys.stderr)
        elif version == newest:
            action = "bump"
        elif version_key(version) > version_key(newest):
            action = "tag"
            print(f"::notice::{slug} {version} was never tagged; tagging it instead of bumping", file=sys.stderr)
        else:
            action = "skip"
            print(f"::warning::{slug}/config.yaml names {version}, older than {slug}/v{newest}", file=sys.stderr)
        print(f"{slug} {action}")
    return 0


def bump(slug):
    config = Path(slug, "config.yaml")
    text = config.read_text()
    m = VERSION_RE.search(text)
    if not m:
        sys.exit(f"{config}: no X.Y.Z version line")
    old = m.group(2)
    since = newest_tag(slug)
    if since != old:
        sys.exit(f"{config}: version {old} is not the newest tag ({since}); run `plan` first")
    major, minor, patch = version_key(old)
    new = f"{major}.{minor}.{patch + 1}"
    config.write_text(VERSION_RE.sub(lambda m: m.group(1) + new + m.group(3), text, count=1))
    if run("yq", "-r", ".version", str(config)).strip() != new:
        sys.exit(f"{config}: the version did not read back as {new}")

    dockerfile = Path(slug, "Dockerfile")
    if dockerfile.exists():
        text = dockerfile.read_text()
        d = BUILD_VERSION_RE.search(text)
        if d:
            if d.group(1) != old:
                sys.exit(f"{dockerfile}: BUILD_VERSION {d.group(1)} differs from config.yaml {old}")
            dockerfile.write_text(BUILD_VERSION_RE.sub(f"ARG BUILD_VERSION={new}", text, count=1))

    subjects = run("git", "log", "--no-merges", "--format=%s", f"{slug}/v{old}..HEAD", "--", f"{slug}/").splitlines()
    log = Path(slug, "CHANGELOG.md")
    date = dt.datetime.now(dt.timezone.utc).date().isoformat()
    text, functional = changelog(log.read_text() if log.exists() else "# Changelog\n", new, old, date, subjects)
    log.write_text(text)
    print(f"{slug}: {old} -> {new}" + (", with unreleased changes" if functional else ""), file=sys.stderr)
    print(f"title=fix({slug}): rebuild on current base packages as {new}")
    print(f"functional={'true' if functional else 'false'}")
    return 0


def main(argv):
    if len(argv) >= 2 and argv[0] == "plan":
        return plan(argv[1:])
    if len(argv) == 2 and argv[0] == "bump":
        return bump(argv[1])
    print(__doc__.split("\n\n")[1], file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
