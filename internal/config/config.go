// Package config loads and validates the agent's settings from a JSON file
// and the environment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	DefaultEndpoint        = "https://ingest.tailcove.dev"
	DefaultMetricsInterval = 30
	MinMetricsInterval     = 10
	DefaultStateFile       = "/var/lib/tailcove-agent/state.json"
)

// LogSource is one file or glob to follow.
type LogSource struct {
	Path string `json:"path"`
	// FromBeginning reads a file from its first line the first time it is
	// seen. By default only lines written after the agent starts are sent.
	FromBeginning bool `json:"from_beginning"`
}

// Metrics controls host metric collection.
type Metrics struct {
	Enabled         *bool    `json:"enabled"`
	IntervalSeconds int      `json:"interval_seconds"`
	ExcludeIfaces   []string `json:"exclude_interfaces"`
	ExcludeMounts   []string `json:"exclude_mounts"`
}

// Config is the whole configuration.
type Config struct {
	Token     string      `json:"token"`
	Endpoint  string      `json:"endpoint"`
	Host      string      `json:"host"`
	Insecure  bool        `json:"insecure"`
	StateFile string      `json:"state_file"`
	Logs      []LogSource `json:"logs"`
	Metrics   Metrics     `json:"metrics"`
}

// MetricsEnabled reports whether host metrics are collected (on by default).
func (c Config) MetricsEnabled() bool {
	return c.Metrics.Enabled == nil || *c.Metrics.Enabled
}

// Load reads the JSON file at path (when it is not empty), applies the
// environment on top, fills defaults and validates the result.
func Load(path string, hostname func() (string, error)) (Config, error) {
	var cfg Config

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("reading config: %w", err)
		}

		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()

		if err := decoder.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parsing config %s: %w", path, err)
		}
	}

	if err := applyEnv(&cfg, os.Getenv); err != nil {
		return cfg, err
	}

	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}

	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")

	if cfg.Metrics.IntervalSeconds == 0 {
		cfg.Metrics.IntervalSeconds = DefaultMetricsInterval
	}

	if cfg.StateFile == "" {
		cfg.StateFile = DefaultStateFile
	}

	if cfg.Host == "" && hostname != nil {
		if name, err := hostname(); err == nil {
			cfg.Host = name
		}
	}

	return cfg, cfg.Validate()
}

func applyEnv(cfg *Config, get func(string) string) error {
	if v := get("TAILCOVE_TOKEN"); v != "" {
		cfg.Token = v
	}

	if v := get("TAILCOVE_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}

	if v := get("TAILCOVE_HOST"); v != "" {
		cfg.Host = v
	}

	if v := get("TAILCOVE_STATE_FILE"); v != "" {
		cfg.StateFile = v
	}

	if v := get("TAILCOVE_LOG_PATHS"); v != "" {
		cfg.Logs = nil

		for _, path := range strings.Split(v, ",") {
			if path = strings.TrimSpace(path); path != "" {
				cfg.Logs = append(cfg.Logs, LogSource{Path: path})
			}
		}
	}

	if v := get("TAILCOVE_METRICS"); v != "" {
		enabled, err := ParseBool(v)
		if err != nil {
			return fmt.Errorf("TAILCOVE_METRICS: %w", err)
		}

		cfg.Metrics.Enabled = &enabled
	}

	if v := get("TAILCOVE_METRICS_INTERVAL"); v != "" {
		seconds, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("TAILCOVE_METRICS_INTERVAL: %q is not a whole number of seconds", v)
		}

		cfg.Metrics.IntervalSeconds = seconds
	}

	if v := get("TAILCOVE_INSECURE"); v != "" {
		insecure, err := ParseBool(v)
		if err != nil {
			return fmt.Errorf("TAILCOVE_INSECURE: %w", err)
		}

		cfg.Insecure = insecure
	}

	return nil
}

// ParseBool accepts exactly true, false, 1 and 0. A flag that switches
// something off must not be read loosely: "no", "off" or a typo is an error,
// not a silent default.
func ParseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}

	return false, fmt.Errorf("%q is not true, false, 1 or 0", value)
}

// Validate checks the settings the agent cannot run without.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("a source token is required (token in the config file, or TAILCOVE_TOKEN)")
	}

	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return fmt.Errorf("endpoint %q is not an http(s) URL", c.Endpoint)
	}

	if endpoint.Scheme == "http" && !c.Insecure && !isLoopback(endpoint.Hostname()) {
		return fmt.Errorf("endpoint %q is plain http: the token would travel in the clear (use https, or set insecure to accept that)", c.Endpoint)
	}

	if c.Metrics.IntervalSeconds < MinMetricsInterval {
		return fmt.Errorf("metrics interval_seconds must be at least %d", MinMetricsInterval)
	}

	for _, source := range c.Logs {
		if strings.TrimSpace(source.Path) == "" {
			return errors.New("a logs entry has no path")
		}
	}

	if len(c.Logs) == 0 && !c.MetricsEnabled() {
		return errors.New("nothing to do: no log files are configured and metrics are off")
	}

	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
