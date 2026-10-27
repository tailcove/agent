package config

import (
	"errors"
	"fmt"
	"strings"
)

// Journald follows systemd's journal (Linux with systemd).
type Journald struct {
	Enabled bool `json:"enabled"`
	// Units limit the entries to these systemd units (globs such as "nginx*" are allowed); empty follows everything.
	Units []string `json:"units"`
	// MinPriority is the least important level that is sent: debug (everything, the default), info, notice, warning, error, critical, alert or emergency.
	MinPriority string `json:"min_priority"`
	// Directory reads another directory's journal files (a copied journal, a host's /var/log/journal mounted into a container).
	Directory string `json:"directory"`
	// Path is the journalctl program; "journalctl" is looked up on the PATH.
	Path string `json:"path"`
	// FromBeginning reads the whole journal the first time; otherwise only what is written from then on.
	FromBeginning bool `json:"from_beginning"`
}

// Docker follows the logs of the containers on this host through the engine's
// socket. Access to the socket is root-equivalent, so it is off unless asked for.
type Docker struct {
	Enabled bool `json:"enabled"`
	// Socket is the engine's unix socket.
	Socket string `json:"socket"`
	// Host is what an entry's host is: container (default), host (this machine) or service (the Compose service).
	Host string `json:"host"`
	// Labels are container labels kept in an entry's context, besides the Compose project and service.
	Labels []string `json:"labels"`
	// Include and Exclude are globs on container names; when Include is set only matching containers are followed.
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
	// MaxContainers bounds how many containers are followed at once.
	MaxContainers int  `json:"max_containers"`
	FromBeginning bool `json:"from_beginning"`
	// Multiline defaults to auto: stack traces are joined into one entry.
	Multiline *Multiline `json:"multiline"`
}

// Defaults of the journald and Docker settings.
const (
	DefaultDockerSocket        = "/var/run/docker.sock"
	DefaultDockerMaxContainers = 200
	MaxDockerContainers        = 2000
)

func (j *Journald) applyDefaults() {
	if j.Path == "" {
		j.Path = "journalctl"
	}

	if j.MinPriority == "" {
		j.MinPriority = "debug"
	}
}

func (d *Docker) applyDefaults() {
	if d.Socket == "" {
		d.Socket = DefaultDockerSocket
	}

	if d.Host == "" {
		d.Host = "container"
	}

	if d.MaxContainers == 0 {
		d.MaxContainers = DefaultDockerMaxContainers
	}
}

func applyContainerEnv(cfg *Config, get func(string) string) error {
	if v := get("TAILCOVE_JOURNALD"); v != "" {
		enabled, err := ParseBool(v)
		if err != nil {
			return fmt.Errorf("TAILCOVE_JOURNALD: %w", err)
		}

		cfg.Journald.Enabled = enabled
	}

	if v := get("TAILCOVE_JOURNALD_UNITS"); v != "" {
		cfg.Journald.Units = splitList(v)
	}

	if v := get("TAILCOVE_JOURNALD_MIN_PRIORITY"); v != "" {
		cfg.Journald.MinPriority = v
	}

	if v := get("TAILCOVE_DOCKER"); v != "" {
		enabled, err := ParseBool(v)
		if err != nil {
			return fmt.Errorf("TAILCOVE_DOCKER: %w", err)
		}

		cfg.Docker.Enabled = enabled
	}

	if v := get("TAILCOVE_DOCKER_SOCKET"); v != "" {
		cfg.Docker.Socket = v
	}

	if v := get("TAILCOVE_DOCKER_HOST"); v != "" {
		cfg.Docker.Host = v
	}

	if v := get("TAILCOVE_DOCKER_FROM_BEGINNING"); v != "" {
		fromBeginning, err := ParseBool(v)
		if err != nil {
			return fmt.Errorf("TAILCOVE_DOCKER_FROM_BEGINNING: %w", err)
		}

		cfg.Docker.FromBeginning = fromBeginning
	}

	for name, target := range map[string]*[]string{
		"TAILCOVE_DOCKER_LABELS":  &cfg.Docker.Labels,
		"TAILCOVE_DOCKER_INCLUDE": &cfg.Docker.Include,
		"TAILCOVE_DOCKER_EXCLUDE": &cfg.Docker.Exclude,
	} {
		if v := get(name); v != "" {
			*target = splitList(v)
		}
	}

	return nil
}

var journalPriorities = map[string]bool{"": true, "debug": true, "info": true, "notice": true, "warning": true, "warn": true, "error": true, "err": true, "critical": true, "crit": true, "alert": true, "emergency": true, "emerg": true}

func (j Journald) validate() error {
	if !j.Enabled {
		return nil
	}

	if !journalPriorities[strings.ToLower(strings.TrimSpace(j.MinPriority))] {
		return fmt.Errorf("journald: min_priority %q is not debug, info, notice, warning, error, critical, alert or emergency", j.MinPriority)
	}

	for _, unit := range j.Units {
		if unit == "" || strings.HasPrefix(unit, "-") || len(unit) > 256 || strings.ContainsAny(unit, " \t\r\n=") {
			return fmt.Errorf("journald: %q is not a unit name or glob", unit)
		}
	}

	if j.Path != "journalctl" && !strings.HasPrefix(j.Path, "/") {
		return errors.New("journald: path must be journalctl or an absolute path")
	}

	if j.Directory != "" && !strings.HasPrefix(j.Directory, "/") {
		return errors.New("journald: directory must be an absolute path")
	}

	return nil
}

func (d Docker) validate() error {
	if !d.Enabled {
		return nil
	}

	if !strings.HasPrefix(d.Socket, "/") {
		return errors.New("docker: socket must be the absolute path of a unix socket")
	}

	if d.Host != "container" && d.Host != "host" && d.Host != "service" {
		return fmt.Errorf("docker: host %q is not container, host or service", d.Host)
	}

	for _, key := range d.Labels {
		if !labelKey.MatchString(key) {
			return fmt.Errorf("docker: %q is not a label key", key)
		}
	}

	if len(d.Labels) > 10 {
		return errors.New("docker: keep at most 10 labels")
	}

	if d.MaxContainers < 1 || d.MaxContainers > MaxDockerContainers {
		return fmt.Errorf("docker: max_containers must be between 1 and %d", MaxDockerContainers)
	}

	return validateMultiline(d.Multiline)
}
