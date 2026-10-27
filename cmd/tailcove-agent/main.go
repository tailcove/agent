// Command tailcove-agent follows log files and reports host metrics to Tailcove.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/tailcove/agent/internal/agent"
	"github.com/tailcove/agent/internal/config"
	"github.com/tailcove/agent/internal/probe"
	"github.com/tailcove/agent/internal/sender"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: tailcove-agent [-config file] <command>

Commands:
  run       follow the configured log files and report host metrics (default)
  check     validate the configuration and test the token against the API
  probe     run as a probe: pull the monitor checks due for this host's region,
            run them from here and post the results (needs a probe token)
  version   print the version

The configuration is a JSON file (-config, or /etc/tailcove-agent.json when it
exists) with environment variables on top: TAILCOVE_TOKEN, TAILCOVE_ENDPOINT,
TAILCOVE_HOST, TAILCOVE_LOG_PATHS (comma separated), TAILCOVE_METRICS (true or
false), TAILCOVE_METRICS_INTERVAL, TAILCOVE_STATE_FILE.

On a Kubernetes node (a DaemonSet), TAILCOVE_KUBERNETES=true follows every pod's
logs under /var/log/pods and reports container and pod CPU and memory; the
"kubernetes" block of the configuration file tunes it (see the docs).

TAILCOVE_JOURNALD=true follows systemd's journal (through journalctl), and
TAILCOVE_DOCKER=true follows the logs of the containers on this host through the
Docker socket. The Docker socket is root-equivalent for whoever can write to it,
so it is off unless asked for; the agent only reads container logs through it.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("tailcove-agent", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to the JSON configuration file")

	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	if err := flags.Parse(args); err != nil {
		return 2
	}

	command := "run"
	if flags.NArg() > 0 {
		command = flags.Arg(0)
	}

	if command == "version" {
		fmt.Printf("tailcove-agent %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)

		return 0
	}

	if command == "probe" {
		return runProbe(*configPath)
	}

	if command != "run" && command != "check" {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", command, usage)

		return 2
	}

	path := *configPath
	if path == "" {
		if _, err := os.Stat("/etc/tailcove-agent.json"); err == nil {
			path = "/etc/tailcove-agent.json"
		}
	}

	cfg, err := config.Load(path, os.Hostname)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tailcove-agent: %v\n", err)

		return 1
	}

	userAgent := "tailcove-agent/" + version
	post := sender.New(cfg.Endpoint, cfg.Token, userAgent)
	post.Logf = log.Printf

	if command == "check" {
		return check(cfg)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("tailcove-agent %s sending to %s as host %q (%d log source(s), metrics %v, kubernetes %v, journald %v, docker %v)", version, cfg.Endpoint, cfg.Host, len(cfg.Logs), cfg.MetricsEnabled(), cfg.Kubernetes.Enabled, cfg.Journald.Enabled, cfg.Docker.Enabled)

	running := &agent.Agent{Config: cfg, Sender: post, State: agent.LoadState(cfg.StateFile), Logf: log.Printf}

	if cfg.Kubernetes.Enabled {
		running.Kube = agent.NewKubernetes(cfg.Kubernetes, log.Printf)
		log.Print(running.Kube.Describe())
	}

	running.Run(ctx)

	log.Print("stopped")

	return 0
}

// runProbe is probe mode: it needs only a probe token and an endpoint.
func runProbe(configPath string) int {
	path := configPath
	if path == "" {
		if _, err := os.Stat("/etc/tailcove-agent.json"); err == nil {
			path = "/etc/tailcove-agent.json"
		}
	}

	cfg, err := config.LoadProbe(path, os.Hostname)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tailcove-agent: %v\n", err)

		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("tailcove-agent %s probing for %s", version, cfg.Endpoint)

	p := probe.New(cfg.Endpoint, cfg.Token, version)
	p.Logf = log.Printf
	p.Run(ctx)

	log.Print("stopped")

	return 0
}

// check posts an empty batch, which the API authenticates and accepts without
// storing anything, and says what it learned.
func check(cfg config.Config) int {
	fmt.Printf("endpoint %s, host %q, %d log source(s), metrics %v, kubernetes %v, journald %v, docker %v\n", cfg.Endpoint, cfg.Host, len(cfg.Logs), cfg.MetricsEnabled(), cfg.Kubernetes.Enabled, cfg.Journald.Enabled, cfg.Docker.Enabled)

	if cfg.Docker.Enabled {
		fmt.Printf("docker: reading container logs through %s; access to that socket is root-equivalent, and this agent only reads logs through it\n", cfg.Docker.Socket)
	}

	request, err := http.NewRequest(http.MethodPost, cfg.Endpoint+"/api/v1/logs", strings.NewReader("[]"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "tailcove-agent: %v\n", err)

		return 1
	}

	request.Header.Set("Authorization", "Bearer "+cfg.Token)
	request.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}

	response, err := client.Do(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not reach the API (is the endpoint right, and is the network open?)")

		return 1
	}

	defer response.Body.Close()

	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		fmt.Println("ok: the API accepted the token")

		return 0
	case response.StatusCode == http.StatusUnauthorized:
		fmt.Fprintln(os.Stderr, "the API refused the token (HTTP 401): copy it again from the source's settings")
	default:
		fmt.Fprintf(os.Stderr, "the API answered HTTP %d\n", response.StatusCode)
	}

	return 1
}
