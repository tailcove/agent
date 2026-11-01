#!/bin/sh
# Runs .github/scripts/next-release.sh against a scratch repository: the next patch after the highest
# stable tag (numerically, so v0.10.0 is above v0.9.0), and nothing for a commit that is tagged, for
# changes that are not code, or when there is no stable tag to count from. Prereleases are not counted.
set -eu

SCRIPT="$(cd "$(dirname "$0")/.." && pwd)/.github/scripts/next-release.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"

git init -q .
git config user.email t@t
git config user.name t

fail() { echo "FAIL: $1" >&2; exit 1; }
expect() { # expect <what> <want>
    got="$(sh "$SCRIPT")"
    [ "$got" = "$2" ] || fail "$1: wanted '$2', got '$got'"
}
commit() { # commit <file> (touches it)
    mkdir -p "$(dirname "$1")"
    echo "$(date +%s%N 2>/dev/null || echo x)$$" >> "$1"
    git add -A
    git commit -q -m "change $1"
}

commit main.go
expect "no stable tag" "skip: there is no stable release tag to count from"

git tag v0.9.0
git tag v0.10.0-rc.0
expect "a tagged commit" "skip: this commit is already tagged"

commit README.md
commit docs/guide.md
commit tests/x_test.sh
commit internal/agent/agent_test.go
commit .github/workflows/ci.yml
expect "docs, tests and workflows only" "skip: only docs, tests or workflows changed since v0.9.0"

commit internal/agent/agent.go
expect "code changed" "v0.9.1"

git tag v0.9.1
commit packaging/build-packages.sh
expect "after v0.9.1" "v0.9.2"

git tag v0.9.2
git tag v0.10.0
commit config.go
expect "numeric order, not text order" "v0.10.1"

git tag v0.10.1
git checkout -q -b old v0.9.0
commit main.go
[ "$(sh "$SCRIPT")" = "skip: v0.10.1 is not behind this commit" ] || fail "a commit that is not after the latest tag"

echo "next-release: ok"
