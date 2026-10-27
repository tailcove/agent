#!/bin/sh
# Builds the packages on the spot and checks what is in them and what their
# maintainer scripts do, with stand-ins for useradd, systemctl and the rest, so
# nothing is installed and no account is created.
set -eu

cd "$(dirname "$0")/.."

command -v dpkg-deb >/dev/null 2>&1 || { echo "package_test: dpkg-deb is needed"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
check() {
    if ! "$@"; then
        echo "FAIL: $*" >&2
        failures=$((failures + 1))
    fi
}

absent() { ! "$@"; }

case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) echo "unsupported architecture"; exit 1 ;;
esac

# ---- the files ------------------------------------------------------------------------------------

sh packaging/build-packages.sh v9.9.9 "$WORK/out" "$ARCH" >/dev/null
DEB="$WORK/out/tailcove-agent_9.9.9_${ARCH}.deb"
check test -f "$DEB"

check test "$(dpkg-deb -f "$DEB" Package)" = tailcove-agent
check test "$(dpkg-deb -f "$DEB" Version)" = 9.9.9
check test "$(dpkg-deb -f "$DEB" Architecture)" = "$ARCH"
check test -n "$(dpkg-deb -f "$DEB" Maintainer)"

mkdir "$WORK/x"
dpkg-deb -x "$DEB" "$WORK/x"
dpkg-deb -e "$DEB" "$WORK/ctl"

check test -x "$WORK/x/usr/bin/tailcove-agent"
check test "$("$WORK/x/usr/bin/tailcove-agent" version | cut -d' ' -f2)" = 9.9.9
check test -f "$WORK/x/usr/share/tailcove-agent/tailcove-agent.example.json"
check grep -q 'ExecStart=/usr/bin/tailcove-agent' "$WORK/x/lib/systemd/system/tailcove-agent.service"
check grep -q 'ExecStart=/usr/bin/tailcove-agent' "$WORK/x/lib/systemd/system/tailcove-probe.service"
check test ! -e "$WORK/x/etc/tailcove-agent.json"
check test "$(stat -c %a "$WORK/x/usr/bin/tailcove-agent")" = 755

# Nothing in the package is owned by anyone but root, and every file has a checksum.
check test -z "$(dpkg-deb -c "$DEB" | awk '$2 != "root/root"')"
(cd "$WORK/x" && md5sum -c "$WORK/ctl/md5sums" >/dev/null) || { echo "FAIL: md5sums" >&2; failures=$((failures + 1)); }

for script in postinst prerm postrm; do
    check sh -n "$WORK/ctl/$script"
    check test -x "$WORK/ctl/$script"
done

# The rpm and a checksum of every package are made too, and a prerelease sorts before the release.
check test -f "$WORK/out/SHA256SUMS"
check test "$(grep -c . "$WORK/out/SHA256SUMS")" = 2
(cd "$WORK/out" && sha256sum -c SHA256SUMS >/dev/null) || { echo "FAIL: SHA256SUMS" >&2; failures=$((failures + 1)); }

case "$ARCH" in amd64) RPM_ARCH=x86_64 ;; arm64) RPM_ARCH=aarch64 ;; esac
RPM="$WORK/out/tailcove-agent-9.9.9-1.${RPM_ARCH}.rpm"
check test -f "$RPM"
check test "$(od -An -tx1 -N4 "$RPM" | tr -d ' \n')" = edabeedb

sh packaging/build-packages.sh v1.2.3-rc.1 "$WORK/rc" "$ARCH" >/dev/null
check test -f "$WORK/rc/tailcove-agent_1.2.3~rc.1_${ARCH}.deb"
check test -f "$WORK/rc/tailcove-agent-1.2.3-0.rc.1.${RPM_ARCH}.rpm"

if sh packaging/build-packages.sh 'v1;rm -rf' "$WORK/bad" "$ARCH" >/dev/null 2>&1; then
    echo "FAIL: a version with shell characters was accepted" >&2
    failures=$((failures + 1))
fi

# With rpm installed (CI runs this in a Fedora container too), it must read the file.
if command -v rpm >/dev/null 2>&1; then
    check rpm -qp --queryformat '%{NAME} %{VERSION} %{RELEASE}\n' "$RPM"
    check rpm -qpl "$RPM"
    check rpm -K "$RPM"
fi

# ---- the maintainer scripts -----------------------------------------------------------------------

STUBS="$WORK/stubs"
mkdir -p "$STUBS"
CALLS="$WORK/calls"

for tool in useradd groupadd usermod userdel groupdel systemctl chgrp; do
    cat > "$STUBS/$tool" <<STUB
#!/bin/sh
echo "$tool \$*" >> "$CALLS"
[ "$tool" = useradd ] && touch "$WORK/user-exists"
exit 0
STUB
    chmod +x "$STUBS/$tool"
done

# getent: the groups adm and systemd-journal exist, docker exists, the service's own group only once created.
cat > "$STUBS/getent" <<STUB
#!/bin/sh
case "\$2" in adm | systemd-journal | docker) exit 0 ;; esac
[ "\$2" = tailcove-agent ] && grep -q groupadd "$CALLS" 2>/dev/null && exit 0
exit 2
STUB
cat > "$STUBS/id" <<STUB
#!/bin/sh
[ -e "$WORK/user-exists" ]
STUB
chmod +x "$STUBS/getent" "$STUBS/id"

run_script() {
    # $1 script, then its arguments
    script="$1"
    shift

    PATH="$STUBS:$PATH" TC_ROOT="$ROOT" sh "$WORK/ctl/$script" "$@" >"$WORK/out.txt" 2>&1
}

new_root() {
    ROOT="$WORK/root-$1"
    rm -f "$CALLS" "$WORK/user-exists"
    mkdir -p "$ROOT/etc" "$ROOT/usr/share/tailcove-agent" "$ROOT/run/systemd/system" "$ROOT/var/lib/tailcove-agent"
    cp "$WORK/x/usr/share/tailcove-agent/tailcove-agent.example.json" "$ROOT/usr/share/tailcove-agent/"
    : > "$CALLS"
}

# 1. A first install creates the user and the config, and does not start a service that has no token.
new_root first
run_script postinst configure
check test -f "$ROOT/etc/tailcove-agent.json"
check test "$(stat -c %a "$ROOT/etc/tailcove-agent.json")" = 640
check grep -q 'groupadd --system tailcove-agent' "$CALLS"
check grep -q 'useradd --system' "$CALLS"
check grep -q 'usermod -a -G adm tailcove-agent' "$CALLS"
check grep -q 'usermod -a -G systemd-journal tailcove-agent' "$CALLS"
check grep -q 'systemctl daemon-reload' "$CALLS"
check absent grep -q 'systemctl start' "$CALLS"
check absent grep -q 'systemctl enable' "$CALLS"
check grep -q 'source token' "$WORK/out.txt"

# The docker group is never granted by a package.
check absent grep -q 'docker' "$CALLS"

# 2. With a token the service is enabled and started on a first install, and only restarted on an upgrade.
new_root token
sed 's/YOUR_SOURCE_TOKEN/real-token/' "$WORK/x/usr/share/tailcove-agent/tailcove-agent.example.json" > "$ROOT/etc/tailcove-agent.json"
run_script postinst configure
check grep -q 'systemctl enable tailcove-agent' "$CALLS"
check grep -q 'systemctl start tailcove-agent' "$CALLS"
check grep -q 'real-token' "$ROOT/etc/tailcove-agent.json"

new_root upgrade
sed 's/YOUR_SOURCE_TOKEN/real-token/' "$WORK/x/usr/share/tailcove-agent/tailcove-agent.example.json" > "$ROOT/etc/tailcove-agent.json"
run_script postinst configure 9.9.8
check grep -q 'systemctl try-restart tailcove-agent' "$CALLS"
check absent grep -q 'systemctl start' "$CALLS"

# 3. An existing configuration is never overwritten.
check grep -q 'real-token' "$ROOT/etc/tailcove-agent.json"

# 4. Without systemd running (a container) nothing calls systemctl, and nothing fails.
new_root nosystemd
rmdir "$ROOT/run/systemd/system"
run_script postinst configure
check absent grep -q systemctl "$CALLS"
check test -f "$ROOT/etc/tailcove-agent.json"

# 5. Removal stops and disables the service and keeps the configuration; a purge removes what the agent wrote.
new_root remove
echo '{"token":"keep"}' > "$ROOT/etc/tailcove-agent.json"
touch "$ROOT/var/lib/tailcove-agent/state.json"
run_script prerm remove
check grep -q 'systemctl stop tailcove-agent' "$CALLS"
check grep -q 'systemctl disable tailcove-agent' "$CALLS"
run_script postrm remove
check test -f "$ROOT/etc/tailcove-agent.json"
touch "$WORK/user-exists"
run_script postrm purge
check test ! -e "$ROOT/etc/tailcove-agent.json"
check test ! -e "$ROOT/var/lib/tailcove-agent"
check grep -q 'userdel tailcove-agent' "$CALLS"

# 6. An upgrade does not stop the service (the new package restarts it).
new_root upgradekeeps
run_script prerm upgrade 9.9.9
check absent grep -q systemctl "$CALLS"

if [ "$failures" -gt 0 ]; then
    echo "$failures check(s) failed" >&2
    exit 1
fi

echo "package tests passed"
