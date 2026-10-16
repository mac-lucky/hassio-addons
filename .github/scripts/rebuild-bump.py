#!/usr/bin/env python3
"""Bump the patch version of every add-on one of the given GHCR images
belongs to, so a rebuild ships as a new version: config.yaml `version:`, the
Dockerfile's `ARG BUILD_VERSION=` default where it has one, and a CHANGELOG
entry. The Supervisor offers an update only for a new version, so a rebuild
under the same one would reach nobody.

Usage: rebuild-bump.py <image>...    (from the repo root, tags fetched)

An image belongs to an add-on when it is the add-on's config.yaml `image:`
with `{arch}` replaced by one of its `arch:` entries, without the registry
(hassio-addon-vector-amd64). Prints the commit title on stdout; exits 1 when
no image belongs to an add-on here.

The CHANGELOG entry takes over whatever the add-on's `[Unreleased]` section
holds. Without one, it lists the subjects of the commits that touched the
add-on since its newest tag, since those ship with the rebuild too.
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


def changelog(text, new, since, date, subjects):
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
    if unreleased:
        return text[:unreleased.start(1)] + "\nNothing yet.\n\n" + section + text[unreleased.end(1):]
    first = re.search(r"^## \[", text, re.M)
    if first:
        return text[:first.start()] + section + text[first.start():]
    return text.rstrip("\n") + "\n\n" + section.rstrip("\n") + "\n"


def bump(slug, date):
    config = Path(slug, "config.yaml")
    text = config.read_text()
    m = VERSION_RE.search(text)
    if not m:
        sys.exit(f"{config}: no X.Y.Z version line")
    old = m.group(2)
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

    since = newest_tag(slug)
    subjects = []
    if since:
        subjects = run("git", "log", "--no-merges", "--format=%s", f"{slug}/v{since}..HEAD", "--", f"{slug}/").splitlines()
    log = Path(slug, "CHANGELOG.md")
    log.write_text(changelog(log.read_text() if log.exists() else "# Changelog\n", new, since or old, date, subjects))
    print(f"{slug}: {old} -> {new}", file=sys.stderr)
    return new


def main(images):
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
    date = dt.datetime.now(dt.timezone.utc).date().isoformat()
    versions = [f"{slug} {bump(slug, date)}" for slug in picked]
    print(f"chore({','.join(picked)}): rebuild on current base packages as {', '.join(versions)}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
