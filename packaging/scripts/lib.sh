# Shared by the maintainer scripts of the .deb and the .rpm (build-packages.sh
# puts this text at the top of each of them). POSIX sh, and every step is
# written to be run again: an upgrade runs the same steps as an install.
#
# TC_ROOT is a prefix for the paths, for the tests; it is empty on a real machine.

TC_USER=tailcove-agent
TC_CONFIG="${TC_ROOT:-}/etc/tailcove-agent.json"
TC_EXAMPLE="${TC_ROOT:-}/usr/share/tailcove-agent/tailcove-agent.example.json"
TC_STATE="${TC_ROOT:-}/var/lib/tailcove-agent"

tc_has_systemd() {
    command -v systemctl >/dev/null 2>&1 && [ -d "${TC_ROOT:-}/run/systemd/system" ]
}

# A system user without a login, in the groups that can read the logs and the
# journal. adm and systemd-journal exist on most distributions; a group that is
# not there is skipped. docker is never added: that group is root-equivalent, and
# whoever wants the agent to read container logs adds it on purpose.
tc_create_user() {
    getent group "$TC_USER" >/dev/null 2>&1 || groupadd --system "$TC_USER"

    if ! id "$TC_USER" >/dev/null 2>&1; then
        shell="$(command -v nologin 2>/dev/null || echo /bin/false)"
        useradd --system --gid "$TC_USER" --home-dir "$TC_STATE" --no-create-home --shell "$shell" "$TC_USER"
    fi

    for group in adm systemd-journal; do
        if getent group "$group" >/dev/null 2>&1; then
            usermod -a -G "$group" "$TC_USER" 2>/dev/null || true
        fi
    done
}

# The configuration is written once, from the example, and never touched again.
# It holds the token, so only root and the service can read it.
tc_write_config() {
    if [ ! -e "$TC_CONFIG" ] && [ -f "$TC_EXAMPLE" ]; then
        install -m 0640 "$TC_EXAMPLE" "$TC_CONFIG"
        chgrp "$TC_USER" "$TC_CONFIG" 2>/dev/null || true
    fi
}

tc_has_token() {
    [ -f "$TC_CONFIG" ] && ! grep -q 'YOUR_SOURCE_TOKEN' "$TC_CONFIG"
}

# $1 is the version that was installed before, and empty on a first install.
tc_post_install() {
    tc_create_user
    tc_write_config

    if ! tc_has_systemd; then
        return 0
    fi

    systemctl daemon-reload || true

    if tc_has_token; then
        systemctl enable tailcove-agent >/dev/null 2>&1 || true

        if [ -n "${1:-}" ]; then
            systemctl try-restart tailcove-agent || true
        else
            systemctl start tailcove-agent || true
        fi
    else
        echo "tailcove-agent: put your source token in /etc/tailcove-agent.json, then run: systemctl enable --now tailcove-agent"
    fi
}

tc_pre_remove() {
    if tc_has_systemd; then
        systemctl stop tailcove-agent >/dev/null 2>&1 || true
        systemctl disable tailcove-agent >/dev/null 2>&1 || true
    fi
}

tc_reload() {
    if tc_has_systemd; then
        systemctl daemon-reload || true
    fi
}

# Only a purge (Debian) removes what the agent wrote itself: its state and
# configuration, and the user.
tc_purge() {
    rm -f "$TC_CONFIG"
    rm -rf "$TC_STATE"

    if id "$TC_USER" >/dev/null 2>&1; then
        userdel "$TC_USER" 2>/dev/null || true
    fi

    getent group "$TC_USER" >/dev/null 2>&1 && groupdel "$TC_USER" 2>/dev/null || true
}
