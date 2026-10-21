#!/bin/sh
# Runs install.sh against a release built on the spot, under a scratch root, so
# the download, the checksum check and the file layout are tested without
# touching the machine or the network.
set -eu

cd "$(dirname "$0")/.."

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) echo "unsupported architecture"; exit 1 ;;
esac

VERSION=9.9.9
RELEASE="$WORK/release"
mkdir -p "$RELEASE"

CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$VERSION" -o "$WORK/tailcove-agent" ./cmd/tailcove-agent
tar -czf "$RELEASE/tailcove-agent_${VERSION}_linux_${ARCH}.tar.gz" -C "$WORK" tailcove-agent
(cd "$RELEASE" && sha256sum "tailcove-agent_${VERSION}_linux_${ARCH}.tar.gz" >checksums.txt)

run_install() {
    TAILCOVE_ROOT="$1" TAILCOVE_VERSION="$VERSION" TAILCOVE_RELEASE_BASE="file://$RELEASE" \
        TAILCOVE_UNIT_URL="file://$PWD/packaging/tailcove-agent.service" sh ./install.sh
}

failures=0
check() {
    if ! "$@"; then
        echo "FAIL: $*" >&2
        failures=$((failures + 1))
    fi
}

# 1. A good release installs the binary, a private config and the unit.
ROOT1="$WORK/root1"
TAILCOVE_TOKEN=secret-token TAILCOVE_LOG_PATHS="/var/log/a.log,/var/log/b/*.log" run_install "$ROOT1" >/dev/null
check test -x "$ROOT1/usr/local/bin/tailcove-agent"
check test "$("$ROOT1/usr/local/bin/tailcove-agent" version | cut -d' ' -f2)" = "$VERSION"
check grep -q '"token": "secret-token"' "$ROOT1/etc/tailcove-agent.json"
check grep -q '"path": "/var/log/b/\*.log"' "$ROOT1/etc/tailcove-agent.json"
check test "$(stat -c %a "$ROOT1/etc/tailcove-agent.json")" = 600
check grep -q 'ExecStart=/usr/local/bin/tailcove-agent' "$ROOT1/etc/systemd/system/tailcove-agent.service"

# 2. A download that does not match its checksum installs nothing.
ROOT2="$WORK/root2"
printf 'tampered' >>"$RELEASE/tailcove-agent_${VERSION}_linux_${ARCH}.tar.gz"
if TAILCOVE_TOKEN=t run_install "$ROOT2" >/dev/null 2>"$WORK/err2"; then
    echo "FAIL: a tampered download was installed" >&2
    failures=$((failures + 1))
fi
check grep -q 'checksum' "$WORK/err2"
check test ! -e "$ROOT2/usr/local/bin/tailcove-agent"

# 3. No token and no config is refused before anything is downloaded.
if run_install "$WORK/root3" >/dev/null 2>"$WORK/err3"; then
    echo "FAIL: installed without a token" >&2
    failures=$((failures + 1))
fi
check grep -q 'TAILCOVE_TOKEN' "$WORK/err3"

# 4. An existing config is never overwritten.
(cd "$RELEASE" && head -c -8 "tailcove-agent_${VERSION}_linux_${ARCH}.tar.gz" >x && mv x "tailcove-agent_${VERSION}_linux_${ARCH}.tar.gz" && sha256sum "tailcove-agent_${VERSION}_linux_${ARCH}.tar.gz" >checksums.txt)
ROOT4="$WORK/root4"
mkdir -p "$ROOT4/etc"
echo '{"token":"mine"}' >"$ROOT4/etc/tailcove-agent.json"
TAILCOVE_TOKEN=other run_install "$ROOT4" >/dev/null
check grep -q '"token":"mine"' "$ROOT4/etc/tailcove-agent.json"

if [ "$failures" -ne 0 ]; then
    echo "$failures check(s) failed" >&2
    exit 1
fi

echo "install.sh: all checks passed"
