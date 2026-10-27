#!/bin/sh
# Builds the .deb and .rpm packages of tailcove-agent for amd64 and arm64.
#
#   packaging/build-packages.sh 0.2.0 [outdir] [arch ...]
#
# VERSION may carry a v and a prerelease (v0.2.0-rc.0). The packages are written to
# outdir (default dist/packages) with a SHA256SUMS file. Needs Go and dpkg-deb;
# the rpm is written by packaging/mkrpm, so rpmbuild is not needed.
set -eu

cd "$(dirname "$0")/.."

[ $# -ge 1 ] || { echo "usage: $0 VERSION [outdir] [arch ...]" >&2; exit 2; }

RAW="${1#v}"
OUT="${2:-dist/packages}"
[ $# -ge 2 ] && shift 2 || shift $#
ARCHES="${*:-amd64 arm64}"

case "$RAW" in
    *[!0-9A-Za-z.+~-]* | "") echo "build-packages: '$RAW' is not a version" >&2; exit 2 ;;
esac

# 0.2.0-rc.0 is older than 0.2.0 in both formats, spelled their own way.
case "$RAW" in
    *-*) BASE="${RAW%%-*}"; PRE="${RAW#*-}"; DEB_VERSION="$BASE~$PRE"; RPM_VERSION="$BASE"; RPM_RELEASE="0.$(printf '%s' "$PRE" | tr -- '-' '.')" ;;
    *) DEB_VERSION="$RAW"; RPM_VERSION="$RAW"; RPM_RELEASE=1 ;;
esac

command -v dpkg-deb >/dev/null 2>&1 || { echo "build-packages: dpkg-deb is needed" >&2; exit 1; }

SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || date +%s)}"
export SOURCE_DATE_EPOCH

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

SUMMARY="Send logs and host metrics to Tailcove"
DESCRIPTION="The Tailcove agent follows your log files, the systemd journal and (when enabled) Docker container logs, and reports CPU, memory, disk and network figures to your Tailcove source. One static binary, a systemd service, no dependencies."

# The unit shipped in the repository points at /usr/local/bin (install.sh); the packages install to /usr/bin.
sed 's|/usr/local/bin/tailcove-agent|/usr/bin/tailcove-agent|' packaging/tailcove-agent.service > "$WORK/tailcove-agent.service"
sed 's|/usr/local/bin/tailcove-agent|/usr/bin/tailcove-agent|' packaging/tailcove-probe.service > "$WORK/tailcove-probe.service"

scriptlet() {
    # $1 output, $2 body
    { printf '#!/bin/sh\nset -e\n'; cat packaging/scripts/lib.sh; printf '\n%s\n' "$2"; } > "$1"
    chmod 0755 "$1"
}

for ARCH in $ARCHES; do
    case "$ARCH" in
        amd64) RPM_ARCH=x86_64 ;;
        arm64) RPM_ARCH=aarch64 ;;
        *) echo "build-packages: unsupported architecture $ARCH" >&2; exit 2 ;;
    esac

    BIN="$WORK/tailcove-agent-$ARCH"
    CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "-s -w -X main.version=$RAW" -o "$BIN" ./cmd/tailcove-agent

    # ---- .deb ----
    TREE="$WORK/deb-$ARCH"
    mkdir -p "$TREE/DEBIAN" "$TREE/usr/bin" "$TREE/lib/systemd/system" "$TREE/usr/share/tailcove-agent" "$TREE/usr/share/doc/tailcove-agent"
    install -m 0755 "$BIN" "$TREE/usr/bin/tailcove-agent"
    install -m 0644 "$WORK/tailcove-agent.service" "$TREE/lib/systemd/system/tailcove-agent.service"
    install -m 0644 "$WORK/tailcove-probe.service" "$TREE/lib/systemd/system/tailcove-probe.service"
    install -m 0644 tailcove-agent.example.json "$TREE/usr/share/tailcove-agent/tailcove-agent.example.json"
    install -m 0644 README.md "$TREE/usr/share/doc/tailcove-agent/README.md"
    install -m 0644 LICENSE "$TREE/usr/share/doc/tailcove-agent/copyright"

    SIZE="$(du -sk "$TREE" | cut -f1)"
    cat > "$TREE/DEBIAN/control" <<CONTROL
Package: tailcove-agent
Version: $DEB_VERSION
Architecture: $ARCH
Maintainer: AKCybex <developers@akcybex.com>
Installed-Size: $SIZE
Section: admin
Priority: optional
Homepage: https://tailcove.dev/docs/sending-logs/agent
Recommends: systemd
Description: $SUMMARY
 The Tailcove agent follows your log files, the systemd journal and, when
 enabled, Docker container logs, and reports CPU, memory, disk and network
 figures to your Tailcove source. One static binary, a systemd service, no
 dependencies. Put your source token in /etc/tailcove-agent.json.
CONTROL

    scriptlet "$TREE/DEBIAN/postinst" 'case "$1" in configure) tc_post_install "${2:-}" ;; esac
exit 0'
    scriptlet "$TREE/DEBIAN/prerm" 'case "$1" in remove | deconfigure) tc_pre_remove ;; esac
exit 0'
    scriptlet "$TREE/DEBIAN/postrm" 'case "$1" in purge) tc_purge ;; remove) tc_reload ;; esac
exit 0'

    (cd "$TREE" && find . -type f ! -path './DEBIAN/*' -print0 | sort -z | xargs -0 md5sum | sed 's| \./| |') > "$TREE/DEBIAN/md5sums"

    # Files get the epoch of the last commit so that the same source gives the same package.
    find "$TREE" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +
    dpkg-deb --root-owner-group -Zxz --build "$TREE" "$OUT/tailcove-agent_${DEB_VERSION}_${ARCH}.deb" >/dev/null

    # ---- .rpm ----
    RPM="$WORK/rpm-$ARCH"
    mkdir -p "$RPM"
    scriptlet "$RPM/post" 'tc_post_install "$([ "${1:-1}" -gt 1 ] && echo upgrade || true)"'
    scriptlet "$RPM/preun" '[ "${1:-0}" -ne 0 ] || tc_pre_remove'
    scriptlet "$RPM/postun" 'tc_reload'

    cat > "$RPM/files.json" <<MANIFEST
[
  {"src": "$BIN", "dest": "/usr/bin/tailcove-agent", "mode": "755"},
  {"src": "$WORK/tailcove-agent.service", "dest": "/usr/lib/systemd/system/tailcove-agent.service", "mode": "644"},
  {"src": "$WORK/tailcove-probe.service", "dest": "/usr/lib/systemd/system/tailcove-probe.service", "mode": "644"},
  {"dest": "/usr/share/tailcove-agent", "mode": "755", "dir": true},
  {"src": "tailcove-agent.example.json", "dest": "/usr/share/tailcove-agent/tailcove-agent.example.json", "mode": "644"},
  {"dest": "/usr/share/doc/tailcove-agent", "mode": "755", "dir": true},
  {"src": "README.md", "dest": "/usr/share/doc/tailcove-agent/README.md", "mode": "644", "doc": true},
  {"src": "LICENSE", "dest": "/usr/share/doc/tailcove-agent/LICENSE", "mode": "644", "doc": true}
]
MANIFEST

    go run ./packaging/mkrpm -out "$OUT/tailcove-agent-${RPM_VERSION}-${RPM_RELEASE}.${RPM_ARCH}.rpm" \
        -name tailcove-agent -version "$RPM_VERSION" -release "$RPM_RELEASE" -arch "$RPM_ARCH" \
        -summary "$SUMMARY" -description "$DESCRIPTION" -license MIT -url https://tailcove.dev/docs/sending-logs/agent \
        -vendor AKCybex -packager "AKCybex <developers@akcybex.com>" -time "$SOURCE_DATE_EPOCH" \
        -manifest "$RPM/files.json" -postin "$RPM/post" -preun "$RPM/preun" -postun "$RPM/postun"
done

(cd "$OUT" && ls ./*.deb ./*.rpm | sed 's|^\./||' | sort | xargs sha256sum > SHA256SUMS)

echo "built:"
ls -1 "$OUT"
