#!/usr/bin/env bash
# Tag the version each add-on's config.yaml names (`<slug>/vX.Y.Z`) when that
# tag does not exist yet, which starts the add-on's release-*.yaml.
#
# Usage: tag-versions.sh [slug...]    (from the repo root, tags fetched,
#                                      origin pushable; no slug: every add-on)
#
# Not tagged: a version that is not X.Y.Z, one older than the add-on's newest
# tag, and an add-on with no tag at all (its first release is cut by hand).
set -euo pipefail

if [ "$#" -eq 0 ]; then
  set -- */config.yaml
  set -- "${@%/config.yaml}"
fi

git config user.name "github-actions[bot]"
git config user.email "41898282+github-actions[bot]@users.noreply.github.com"

for slug in "$@"; do
  config="${slug}/config.yaml"
  version=$(yq -r '.version' "$config")
  if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "::warning::$config: version '$version' is not X.Y.Z; not tagged"
    continue
  fi
  tag="${slug}/v${version}"
  if git rev-parse -q --verify "refs/tags/${tag}" > /dev/null; then
    echo "$tag exists"
    continue
  fi
  last=$(git tag --list "${slug}/v*" | grep -E "^${slug}/v[0-9]+\.[0-9]+\.[0-9]+$" | sort -V | tail -n1 || true)
  if [ -z "$last" ]; then
    echo "::notice::$slug has no ${slug}/v* tag yet; cut its first release by hand"
    continue
  fi
  if [ "$(printf '%s\n%s\n' "$last" "$tag" | sort -V | tail -n1)" != "$tag" ]; then
    echo "::warning::$config names $version, older than $last; not tagged"
    continue
  fi
  git tag -a "$tag" -m "$tag"
  # One tag per push: GitHub starts no workflow for a push of more than three
  # tags at once.
  git push origin "refs/tags/${tag}"
  echo "pushed $tag"
done
