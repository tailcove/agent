// Package config loads and validates the agent's settings from a JSON file
// and the environment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
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
	// Multiline joins the lines of one event (a stack trace) into one entry.
	// Off unless set.
	Multiline *Multiline `json:"multiline"`
}

// Multiline says how the lines of one event are told apart: mode auto
// recognises indented lines and the usual stack trace shapes, mode pattern
// treats a line that matches pattern as the start of a new event.
type Multiline struct {
	Mode      string `json:"mode"`
	Pattern   string `json:"pattern"`
	MaxLines  int    `json:"max_lines"`
	MaxBytes  int    `json:"max_bytes"`
	TimeoutMS int    `json:"timeout_ms"`
}

// Metrics controls host metric collection.
type Metrics struct {
	Enabled         *bool    `json:"enabled"`
	IntervalSeconds int      `json:"interval_seconds"`
	ExcludeIfaces   []string `json:"exclude_interfaces"`
	ExcludeMounts   []string `json:"exclude_mounts"`
	// ProcRoot is where the host's /proc is mounted when the agent runs in a
	// container (/host/proc); /proc by default.
	ProcRoot string `json:"proc_root"`
}

// KubernetesRoute sends the pods it matches to another source.
type KubernetesRoute struct {
	// Namespace is a glob; empty matches every namespace.
	Namespace string `json:"namespace"`
	// Labels are all required of the pod.
	Labels map[string]string `json:"labels"`
	// Token is the source token of the source the lines go to.
	Token string `json:"token"`
}

// KubernetesMetrics controls container and pod metrics.
type KubernetesMetrics struct {
	Enabled         *bool  `json:"enabled"`
	IntervalSeconds int    `json:"interval_seconds"`
	CgroupRoot      string `json:"cgroup_root"`
	// MaxSeries bounds the points of one reading.
	MaxSeries int `json:"max_series"`
}

// Kubernetes controls discovery of the pods on this node: their logs from the
// node's log directory, the namespace, pod and labels added to every line, and
// the CPU and memory of their containers.
type Kubernetes struct {
	Enabled bool `json:"enabled"`
	// NodeName is this node (NODE_NAME from the downward API by default).
	NodeName      string `json:"node_name"`
	PodsDir       string `json:"pods_dir"`
	ContainersDir string `json:"containers_dir"`
	FromBeginning bool   `json:"from_beginning"`
	// Host is what a line's host is: pod (default), node, workload or label:<key>.
	Host string `json:"host"`
	// Labels are the pod labels kept in a line's context.
	Labels            []string          `json:"labels"`
	IncludeNamespaces []string          `json:"include_namespaces"`
	ExcludeNamespaces []string          `json:"exclude_namespaces"`
	ExcludePods       []string          `json:"exclude_pods"`
	ExcludeContainers []string          `json:"exclude_containers"`
	Routes            []KubernetesRoute `json:"routes"`
	// API turns off the Kubernetes API lookups (labels and owners) when false.
	API *bool `json:"api"`
	// Multiline defaults to auto for container logs.
	Multiline *Multiline        `json:"multiline"`
	Metrics   KubernetesMetrics `json:"metrics"`
}

// Config is the whole configuration.
type Config struct {
	Token      string      `json:"token"`
	Endpoint   string      `json:"endpoint"`
	Host       string      `json:"host"`
	Insecure   bool        `json:"insecure"`
	StateFile  string      `json:"state_file"`
	Logs       []LogSource `json:"logs"`
	Metrics    Metrics     `json:"metrics"`
	Kubernetes Kubernetes  `json:"kubernetes"`
}

// MetricsEnabled reports whether host metrics are collected (on by default).
func (c Config) MetricsEnabled() bool {
	return c.Metrics.Enabled == nil || *c.Metrics.Enabled
}

// Load reads the JSON file at path (when it is not empty), applies the
// environment on top, fills defaults and validates the result.
func Load(path string, hostname func() (string, error)) (Config, error) {
	cfg, err := load(path, hostname)
	if err != nil {
		return cfg, err
	}

	return cfg, cfg.Validate()
}

// LoadProbe is Load for probe mode: the token is a probe token, and there are
// no log files or metrics to require.
func LoadProbe(path string, hostname func() (string, error)) (Config, error) {
	cfg, err := load(path, hostname)
	if err != nil {
		return cfg, err
	}

	return cfg, cfg.ValidateProbe()
}

func load(path string, hostname func() (string, error)) (Config, error) {
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

	if cfg.Metrics.ProcRoot == "" {
		cfg.Metrics.ProcRoot = "/proc"
	}

	cfg.Kubernetes.applyDefaults(cfg.Metrics.IntervalSeconds, hostname)

	if cfg.Host == "" && hostname != nil {
		if name, err := hostname(); err == nil {
			cfg.Host = name
		}
	}

	return cfg, nil
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

	if err := applyKubernetesEnv(cfg, get); err != nil {
		return err
	}

	if v := get("TAILCOVE_PROC_ROOT"); v != "" {
		cfg.Metrics.ProcRoot = v
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

	if len(c.Logs) == 0 && !c.MetricsEnabled() && !c.Kubernetes.Enabled {
		return errors.New("nothing to do: no log files are configured, metrics are off and Kubernetes discovery is off")
	}

	for i, source := range c.Logs {
		if err := validateMultiline(source.Multiline); err != nil {
			return fmt.Errorf("logs entry %d: %w", i+1, err)
		}
	}

	return c.Kubernetes.validate()
}

// ValidateProbe checks what probe mode cannot run without: a probe token (it
// starts with tcp_, unlike a source token) and an endpoint the token may travel to.
func (c Config) ValidateProbe() error {
	if !strings.HasPrefix(strings.TrimSpace(c.Token), "tcp_") {
		return errors.New("a probe token is required (token in the config file, or TAILCOVE_TOKEN): it starts with tcp_; a source token does not open the probe endpoints")
	}

	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return fmt.Errorf("endpoint %q is not an http(s) URL", c.Endpoint)
	}

	if endpoint.Scheme == "http" && !c.Insecure && !isLoopback(endpoint.Hostname()) {
		return fmt.Errorf("endpoint %q is plain http: the token would travel in the clear (use https, or set insecure to accept that)", c.Endpoint)
	}

	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Defaults of the Kubernetes settings (only used when they are enabled).
const (
	DefaultPodsDir       = "/var/log/pods"
	DefaultContainersDir = "/var/log/containers"
	DefaultCgroupRoot    = "/sys/fs/cgroup"
)

// DefaultKubernetesLabels are the pod labels kept when none are named.
var DefaultKubernetesLabels = []string{"app", "app.kubernetes.io/name", "app.kubernetes.io/component", "app.kubernetes.io/version"}

func (k *Kubernetes) applyDefaults(metricsInterval int, hostname func() (string, error)) {
	if k.PodsDir == "" {
		k.PodsDir = DefaultPodsDir
	}

	if k.ContainersDir == "" {
		k.ContainersDir = DefaultContainersDir
	}

	if k.Host == "" {
		k.Host = "pod"
	}

	if k.Labels == nil {
		k.Labels = append([]string(nil), DefaultKubernetesLabels...)
	}

	if k.Metrics.IntervalSeconds == 0 {
		k.Metrics.IntervalSeconds = metricsInterval
	}

	if k.Metrics.CgroupRoot == "" {
		k.Metrics.CgroupRoot = DefaultCgroupRoot
	}

	if k.NodeName == "" {
		k.NodeName = os.Getenv("NODE_NAME")
	}

	if k.NodeName == "" && hostname != nil {
		if name, err := hostname(); err == nil {
			k.NodeName = name
		}
	}
}

// MetricsEnabled reports whether container and pod metrics are collected (on
// when discovery is on, unless turned off).
func (k Kubernetes) MetricsEnabled() bool {
	return k.Enabled && (k.Metrics.Enabled == nil || *k.Metrics.Enabled)
}

// APIEnabled reports whether the Kubernetes API is asked for labels and owners.
func (k Kubernetes) APIEnabled() bool {
	return k.API == nil || *k.API
}

func applyKubernetesEnv(cfg *Config, get func(string) string) error {
	k := &cfg.Kubernetes

	if v := get("TAILCOVE_KUBERNETES"); v != "" {
		enabled, err := ParseBool(v)
		if err != nil {
			return fmt.Errorf("TAILCOVE_KUBERNETES: %w", err)
		}

		k.Enabled = enabled
	}

	if v := get("TAILCOVE_NODE_NAME"); v != "" {
		k.NodeName = v
	}

	if v := get("TAILCOVE_K8S_HOST"); v != "" {
		k.Host = v
	}

	if v := get("TAILCOVE_K8S_FROM_BEGINNING"); v != "" {
		fromBeginning, err := ParseBool(v)
		if err != nil {
			return fmt.Errorf("TAILCOVE_K8S_FROM_BEGINNING: %w", err)
		}

		k.FromBeginning = fromBeginning
	}

	if v := get("TAILCOVE_CGROUP_ROOT"); v != "" {
		k.Metrics.CgroupRoot = v
	}

	for name, target := range map[string]*[]string{
		"TAILCOVE_K8S_LABELS":             &k.Labels,
		"TAILCOVE_K8S_INCLUDE_NAMESPACES": &k.IncludeNamespaces,
		"TAILCOVE_K8S_EXCLUDE_NAMESPACES": &k.ExcludeNamespaces,
		"TAILCOVE_K8S_EXCLUDE_PODS":       &k.ExcludePods,
		"TAILCOVE_K8S_EXCLUDE_CONTAINERS": &k.ExcludeContainers,
	} {
		if v := get(name); v != "" {
			*target = splitList(v)
		}
	}

	return nil
}

func splitList(value string) []string {
	var items []string

	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}

	return items
}

var labelKey = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

func (k Kubernetes) validate() error {
	if !k.Enabled {
		return nil
	}

	if strings.TrimSpace(k.NodeName) == "" {
		return errors.New("kubernetes: the node name is unknown (set node_name, or NODE_NAME from the downward API)")
	}

	if k.Host != "pod" && k.Host != "node" && k.Host != "workload" {
		key, isLabel := strings.CutPrefix(k.Host, "label:")

		if !isLabel || !labelKey.MatchString(key) {
			return fmt.Errorf("kubernetes: host %q is not pod, node, workload or label:<key>", k.Host)
		}
	}

	for _, key := range k.Labels {
		if !labelKey.MatchString(key) {
			return fmt.Errorf("kubernetes: %q is not a label key", key)
		}
	}

	if len(k.Labels) > 10 {
		return errors.New("kubernetes: keep at most 10 labels")
	}

	for i, route := range k.Routes {
		if strings.TrimSpace(route.Token) == "" || strings.ContainsAny(route.Token, " \t\r\n") {
			return fmt.Errorf("kubernetes: route %d has no token (or one with spaces)", i+1)
		}

		if route.Namespace == "" && len(route.Labels) == 0 {
			return fmt.Errorf("kubernetes: route %d matches everything: name a namespace or labels (the default source is the main token)", i+1)
		}
	}

	if k.Metrics.IntervalSeconds < MinMetricsInterval {
		return fmt.Errorf("kubernetes: metrics interval_seconds must be at least %d", MinMetricsInterval)
	}

	if k.Metrics.MaxSeries < 0 {
		return errors.New("kubernetes: metrics max_series cannot be negative")
	}

	return validateMultiline(k.Multiline)
}

func validateMultiline(m *Multiline) error {
	if m == nil {
		return nil
	}

	switch m.Mode {
	case "", "off", "auto":
	case "pattern":
		if m.Pattern == "" {
			return errors.New("multiline mode pattern needs a pattern: the regular expression that begins an event")
		}

		if _, err := regexp.Compile(m.Pattern); err != nil {
			return fmt.Errorf("multiline pattern: %w", err)
		}
	default:
		return fmt.Errorf("multiline mode %q is not off, auto or pattern", m.Mode)
	}

	if m.MaxLines < 0 || m.MaxBytes < 0 || m.TimeoutMS < 0 {
		return errors.New("multiline limits cannot be negative")
	}

	return nil
}
