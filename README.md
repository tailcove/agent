<a href="https://tailcove.dev/docs/sending-logs/agent"><picture><source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/tailcove/.github/main/profile/assets/packages/agent-dark.svg"><source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/tailcove/.github/main/profile/assets/packages/agent-light.svg"><img alt="tailcove-agent: Tailcove for the agent. Version, runtime support, size and license." src="https://raw.githubusercontent.com/tailcove/.github/main/profile/assets/packages/agent-dark.svg" width="100%"></picture></a>

<p align="center">
  <a href="https://tailcove.dev/docs/sending-logs/agent">Documentation</a> &nbsp;·&nbsp;
  <a href="https://app.tailcove.dev/register">Get started free</a> &nbsp;·&nbsp;
  <a href="https://github.com/tailcove/agent/releases">View releases</a> &nbsp;·&nbsp;
  <a href="https://github.com/tailcove/agent/issues/new/choose">Report a bug</a>
</p>

<h3 align="center">Send your server's logs and host metrics to Tailcove with one small program.</h3>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/tailcove/.github/main/profile/assets/packages/features-agent-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/tailcove/.github/main/profile/assets/packages/features-agent-light.svg">
  <img alt="tailcove-agent features" src="https://raw.githubusercontent.com/tailcove/.github/main/profile/assets/packages/features-agent-dark.svg" width="100%">
</picture>

## Install

On a Linux server with systemd, with a source token from your Tailcove source:

```sh
curl -fsSL https://github.com/tailcove/agent/releases/latest/download/install.sh | \
  TAILCOVE_TOKEN=your-source-token TAILCOVE_LOG_PATHS=/var/log/nginx/*.log sh
```

The script downloads the release for your architecture, checks it against the
release's SHA-256 sums, installs `/usr/local/bin/tailcove-agent`, writes
`/etc/tailcove-agent.json` and starts a systemd service. Or use the container:

```sh
docker run -d --name tailcove-agent --restart always \
  -e TAILCOVE_TOKEN=your-source-token \
  -e TAILCOVE_LOG_PATHS=/var/log/app/*.log \
  -v /var/log/app:/var/log/app:ro \
  ghcr.io/tailcove/agent
```

## Configure

A JSON file (`-config`, or `/etc/tailcove-agent.json`), with environment
variables on top:

```json
{
  "token": "YOUR_SOURCE_TOKEN",
  "logs": [
    { "path": "/var/log/nginx/*.log" },
    { "path": "/var/log/myapp/app.log", "from_beginning": false }
  ],
  "metrics": { "enabled": true, "interval_seconds": 30 }
}
```

- `TAILCOVE_TOKEN`, `TAILCOVE_ENDPOINT` (default `https://ingest.tailcove.dev`),
  `TAILCOVE_HOST` (default: the machine's hostname), `TAILCOVE_LOG_PATHS`
  (comma separated), `TAILCOVE_METRICS` (`true` or `false`),
  `TAILCOVE_METRICS_INTERVAL`, `TAILCOVE_STATE_FILE`.
- Only `true`, `false`, `1` and `0` are accepted for a switch; anything else is
  an error, not a silent default.
- A plain `http` endpoint is refused unless it is on this machine or you set
  `"insecure": true`, because the token would travel in the clear.

`tailcove-agent check` validates the configuration and asks the API whether the
token is accepted. `tailcove-agent version` prints the version.

## Run as a probe

With a **probe token** the same program checks your monitors from the host's region. Tailcove hands it the
HTTP and TCP checks due for that region, it runs them from here and posts the results; a monitor checked from
several regions is down only when enough of them agree.

```sh
TAILCOVE_TOKEN=tcp_your-probe-token tailcove-agent probe
```

A systemd unit for it is in `packaging/tailcove-probe.service`. A probe token starts with `tcp_` and is created by whoever runs your Tailcove; a source token does not open the
probe endpoints. A probe follows no log files and sends no host metrics. It refuses to connect to private,
loopback, link-local and other non-public addresses, at the moment of connecting, and sends a monitor's headers and
body only to the host the monitor names. It holds no other credential.

## Journald, Docker and Kubernetes

**journald** (Linux with systemd). `"journald": {"enabled": true}` or
`TAILCOVE_JOURNALD=true` follows the journal through `journalctl`, which is
read-only and already on the machine, so there is no library to install. Each
entry becomes a log line with its level (from the syslog priority), time, host
and a `journald` context (unit, identifier, pid). Limit it with `units`
(`["nginx.service", "sshd*"]`) and `min_priority` (`warning` sends warnings and
worse). The journal's cursor is saved, so a restart continues after the last
delivered entry. The service user must be allowed to read the journal: the
packages add it to the `systemd-journal` group.

**Docker.** `"docker": {"enabled": true}` or `TAILCOVE_DOCKER=true` follows the
logs of every running container through `/var/run/docker.sock`
(`TAILCOVE_DOCKER_SOCKET` for another). **Access to that socket is
root-equivalent for whoever can write to it**, which is why it is off by
default; the agent itself only lists containers, inspects one and reads its
logs, never starts, stops or runs anything. Entries carry the container, image
and Compose project and service in a `docker` context; `host` is the container
(or `"host": "host"` for this machine, `"service"` for the Compose service).
`include` and `exclude` take container name globs, `labels` names labels to
keep, and stack traces are joined into one entry (`multiline`). Give the service
user access with `usermod -aG docker tailcove-agent` (which is the root-equivalent
step), or run the agent as a container with the socket mounted read-only.

**Kubernetes.** `"kubernetes": {"enabled": true}` or `TAILCOVE_KUBERNETES=true`
on a node follows every pod's logs and reports container and pod CPU and memory;
the DaemonSet manifest and Helm chart are in Tailcove's repository under
`deploy/agent`, and the settings are in the
[docs](https://tailcove.dev/docs/sending-logs/agent#kubernetes).

## What it sends

**Logs.** Every line appended to a followed file becomes a log line with the
host and the file name. A line that is already a JSON object is sent as it is,
so structured logs keep their fields. Files are followed through rotation and
truncation, globs pick up files created later, and where each file got to is
saved, so a restart neither repeats nor skips lines. A new file that was already
there is followed from its end unless you set `from_beginning`. Lines over 64 KB
are cut and marked.

**Metrics.** Every interval (30 seconds by default, at least 10), on Linux:
CPU usage, load, memory, swap, uptime, and per mount and per interface disk and
network figures, named `system.*`. Network and disk figures are counters or
gauges as the kernel reports them; use the `max` or `last` aggregate to chart a
counter. Virtual interfaces (`lo`, `veth*`, `docker*` and so on) are left out by
default, and a mount can be excluded in the configuration. Every distinct mount
and interface is a series that counts toward your plan's series limit.

**Delivery.** Batches go out as soon as 500 lines are waiting or a second has
passed. Failures are retried with growing, jittered waits; a `429` or `503`
waits at least as long as the API's `Retry-After`. A batch the API refuses for
what it holds is dropped, with a log line. While the API is unreachable the
agent stops reading further instead of losing lines: the files stay where they
are.

## Build

```sh
go build ./cmd/tailcove-agent
go test -race ./...
```

Go 1.24 or newer; the standard library is the only dependency.

## Releases

Pushing a `v*` tag runs the publish workflow: it builds the Linux binaries,
attaches them to a GitHub release with `checksums.txt` and `install.sh`, and
pushes `ghcr.io/tailcove/agent`. No registry token is stored anywhere.

## Help and feedback

- **Docs:** [tailcove.dev/docs/sending-logs/agent](https://tailcove.dev/docs/sending-logs/agent)
- **Bugs and feature requests:** [open an issue](https://github.com/tailcove/agent/issues/new/choose)
- **Security problems:** please email **security@tailcove.dev** instead of opening a public issue.

<br>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/tailcove/.github/main/profile/assets/packages/footer-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/tailcove/.github/main/profile/assets/packages/footer-light.svg">
  <img alt="Tailcove. Built by AKCybex." src="https://raw.githubusercontent.com/tailcove/.github/main/profile/assets/packages/footer-dark.svg" width="100%">
</picture>

<p align="center">
  <a href="https://tailcove.dev/docs/sending-logs/agent">Documentation</a> &nbsp;·&nbsp;
  <a href="https://app.tailcove.dev/register">Get started free</a> &nbsp;·&nbsp;
  <a href="https://github.com/tailcove/agent/issues/new/choose">Report a bug</a> &nbsp;·&nbsp;
  <a href="LICENSE">MIT License</a>
</p>

<p align="center">
  <sub>More Tailcove libraries: <a href="https://github.com/tailcove/laravel">Laravel</a> &nbsp;·&nbsp; <a href="https://github.com/tailcove/node">Node.js</a> &nbsp;·&nbsp; <a href="https://github.com/tailcove/python">Python</a> &nbsp;·&nbsp; <a href="https://github.com/tailcove/browser">Browser</a></sub>
</p>
