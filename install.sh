#!/bin/sh
# Installs the Tailcove agent on a Linux server and starts it as a systemd service.
#
#   curl -fsSL https://github.com/tailcove/agent/releases/latest/download/install.sh | \
#     TAILCOVE_TOKEN=your-source-token sh
#
# Optional: TAILCOVE_VERSION (default: the latest release), TAILCOVE_LOG_PATHS
# (comma separated files or globs). The download is checked against the
# release's SHA-256 sums before anything is installed.
set -eu

REPO="tailcove/agent"
# TAILCOVE_ROOT installs under another directory without touching the service
# manager or users (for packaging and for the tests); TAILCOVE_RELEASE_BASE and
# TAILCOVE_UNIT_URL point at a mirror.
ROOT="${TAILCOVE_ROOT:-}"
BIN="$ROOT/usr/local/bin/tailcove-agent"
CONFIG="$ROOT/etc/tailcove-agent.json"
UNIT="$ROOT/etc/systemd/system/tailcove-agent.service"

fail() {
    echo "install: $*" >&2
    exit 1
}

[ "$(uname -s)" = "Linux" ] || fail "only Linux is supported"

if [ -z "$ROOT" ]; then
    [ "$(id -u)" -eq 0 ] || fail "run as root (for example with sudo)"
    command -v systemctl >/dev/null 2>&1 || fail "systemd is required"
fi

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"
[ -n "${TAILCOVE_TOKEN:-}" ] || [ -f "$CONFIG" ] || fail "set TAILCOVE_TOKEN to your source token (or create $CONFIG first)"

case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) fail "unsupported architecture $(uname -m)" ;;
esac

if [ -n "${TAILCOVE_VERSION:-}" ]; then
    VERSION="${TAILCOVE_VERSION#v}"
elif [ -n "${TAILCOVE_RELEASE_BASE:-}" ]; then
    fail "set TAILCOVE_VERSION together with TAILCOVE_RELEASE_BASE"
else
    VERSION="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's#.*/tag/v##')"
    [ -n "$VERSION" ] || fail "could not find the latest release"
fi

ARCHIVE="tailcove-agent_${VERSION}_linux_${ARCH}.tar.gz"
BASE="${TAILCOVE_RELEASE_BASE:-https://github.com/$REPO/releases/download/v$VERSION}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Downloading tailcove-agent $VERSION ($ARCH)"
curl -fsSL -o "$TMP/$ARCHIVE" "$BASE/$ARCHIVE"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt"

(cd "$TMP" && grep " $ARCHIVE\$" checksums.txt | sha256sum -c -) || fail "the download does not match its checksum"

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" tailcove-agent
mkdir -p "$(dirname "$BIN")" "$(dirname "$CONFIG")" "$(dirname "$UNIT")"
install -m 0755 "$TMP/tailcove-agent" "$BIN"

if [ -z "$ROOT" ]; then
    id tailcove-agent >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin tailcove-agent
fi

if [ ! -f "$CONFIG" ]; then
    TOKEN="$TAILCOVE_TOKEN"
    LOGS="${TAILCOVE_LOG_PATHS:-}"

    umask 077
    {
        printf '{\n  "token": "%s",\n  "logs": [' "$TOKEN"
        FIRST=1
        OLD_IFS="$IFS"
        IFS=','
        for LOG in $LOGS; do
            [ -n "$LOG" ] || continue
            [ "$FIRST" -eq 1 ] || printf ','
            printf '\n    { "path": "%s" }' "$LOG"
            FIRST=0
        done
        IFS="$OLD_IFS"
        printf '\n  ]\n}\n'
    } >"$CONFIG"
    [ -n "$ROOT" ] || chown tailcove-agent:tailcove-agent "$CONFIG"
    echo "Wrote $CONFIG"
fi

curl -fsSL -o "$UNIT" "${TAILCOVE_UNIT_URL:-https://raw.githubusercontent.com/$REPO/v$VERSION/packaging/tailcove-agent.service}"

if [ -n "$ROOT" ]; then
    echo "Installed under $ROOT; not starting a service."
    exit 0
fi

systemctl daemon-reload
systemctl enable --now tailcove-agent
"$BIN" -config "$CONFIG" check || echo "install: the agent is running, but the check above failed; fix $CONFIG and run 'systemctl restart tailcove-agent'" >&2

echo "Done. Logs: journalctl -u tailcove-agent -f"
