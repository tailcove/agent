#!/bin/sh
# Says which version a commit on main should be released as, or why it should not be: one line,
# `vX.Y.Z` or `skip: <reason>`. A release is the next patch after the highest stable tag, and only when
# code changed since that tag (docs, tests, workflows and the license alone are not a release). A new
# minor or major version is still a tag pushed by hand, which this then leaves alone.
#
#   sh .github/scripts/next-release.sh [commit]      (default HEAD; needs the tags fetched)
set -eu

head="$(git rev-parse "${1:-HEAD}")"

latest="$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -t. -k1.2n -k2n -k3n | tail -n 1 || true)"
if [ -z "$latest" ]; then
    echo "skip: there is no stable release tag to count from"
    exit 0
fi

if git tag --points-at "$head" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+'; then
    echo "skip: this commit is already tagged"
    exit 0
fi

if ! git merge-base --is-ancestor "$latest" "$head"; then
    echo "skip: $latest is not behind this commit"
    exit 0
fi

changed="$(git diff --name-only "$latest" "$head" | grep -vE '^(.*\.md|LICENSE|.*_test\.go|(docs|tests|\.github)/.*)$' || true)"
if [ -z "$changed" ]; then
    echo "skip: only docs, tests or workflows changed since $latest"
    exit 0
fi

version="${latest#v}"
major="${version%%.*}"
rest="${version#*.}"
minor="${rest%%.*}"
patch="${rest#*.}"

echo "v$major.$minor.$((patch + 1))"
