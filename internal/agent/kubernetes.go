package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tailcove/agent/internal/config"
	"github.com/tailcove/agent/internal/containermetrics"
	"github.com/tailcove/agent/internal/hostmetrics"
	"github.com/tailcove/agent/internal/kube"
	"github.com/tailcove/agent/internal/tail"
)

// pruneEvery is how often the state file is cleaned of pods that are gone.
const pruneEvery = 10 * time.Minute

// Kubernetes is the discovery of the pods on this node: which container logs
// are followed, what each line is told about its pod, where it goes, and the
// CPU and memory of the containers.
type Kubernetes struct {
	Config   config.Kubernetes
	Mapper   *kube.Mapper
	Registry *kube.Registry
	Logf     func(format string, args ...any)
}

// NewKubernetes sets up discovery from the configuration. The Kubernetes API is
// asked for labels and owners only when it can be reached as a pod; where it
// cannot (not running in a cluster, or switched off) lines carry what the
// file's path says, and that is said once.
func NewKubernetes(cfg config.Kubernetes, logf func(format string, args ...any)) *Kubernetes {
	mapper := &kube.Mapper{
		Host:              cfg.Host,
		LabelKeys:         cfg.Labels,
		IncludeNamespaces: cfg.IncludeNamespaces,
		ExcludeNamespaces: cfg.ExcludeNamespaces,
		ExcludePods:       cfg.ExcludePods,
		ExcludeContainers: cfg.ExcludeContainers,
	}

	for _, route := range cfg.Routes {
		mapper.Routes = append(mapper.Routes, kube.Route{Namespace: route.Namespace, Labels: route.Labels, Token: route.Token})
	}

	registry := &kube.Registry{PodsDir: cfg.PodsDir, Node: cfg.NodeName, Mapper: mapper, Logf: logf}

	if cfg.APIEnabled() {
		api, err := kube.InCluster()
		if err != nil {
			if logf != nil {
				logf("kubernetes: %v; lines carry the namespace, pod and container from the file path, without labels or owner", err)
			}
		} else {
			registry.API = api
		}
	}

	return &Kubernetes{Config: cfg, Mapper: mapper, Registry: registry, Logf: logf}
}

// PodsDir is where the node keeps its pods' logs.
func (k *Kubernetes) PodsDir() string { return k.Config.PodsDir }

// Source is the file pattern the follower reads for the pods: every
// <pods dir>/<ns>_<pod>_<uid>/<container>/<n>.log that is not excluded, in the
// runtime's format, with stack traces joined.
func (k *Kubernetes) Source() tail.Source {
	multiline := k.Config.Multiline
	if multiline == nil {
		multiline = &config.Multiline{Mode: "auto"}
	}

	joined, err := tailMultiline(multiline)
	if err != nil && k.Logf != nil {
		k.Logf("kubernetes: ignoring the multiline setting: %v", err)
	}

	podsDir := k.Config.PodsDir

	return tail.Source{
		Pattern:       strings.TrimRight(podsDir, "/") + "/*/*/*.log",
		FromBeginning: k.Config.FromBeginning,
		Format:        tail.FormatAuto,
		Multiline:     joined,
		Accept: func(path string) bool {
			parsed, ok := kube.ParsePodLog(podsDir, path)

			return ok && k.Mapper.Accept(parsed.Namespace, parsed.Pod, parsed.Container)
		},
	}
}

// Encode turns a line of a pod's log into the entry to send, and says which
// source it goes to ("" is the main one). ok is false for a line that is not
// from a pod's log file.
func (k *Kubernetes) Encode(ctx context.Context, line tail.Line, defaultHost string) (json.RawMessage, string, bool) {
	meta, ok := k.Registry.Lookup(ctx, line.File)
	if !ok {
		return nil, "", false
	}

	return kubeEntry(line, meta, k.Mapper), k.Mapper.TokenFor(meta), true
}

// timeKeys are the names under which a structured line already says when it happened.
var timeKeys = []string{"ts", "timestamp", "time", "@timestamp", "datetime", "date"}

// kubeEntry is the JSON object for one line of a pod's log. A line that is
// already a JSON object is passed on as it is, with the host, a time (the
// runtime's, when it names none) and the pod added; any other line becomes a
// message with the pod in its context.
func kubeEntry(line tail.Line, meta kube.Meta, mapper *kube.Mapper) json.RawMessage {
	host := mapper.HostFor(meta)
	kubernetes := mapper.Context(meta)
	text := strings.TrimSpace(line.Text)

	if strings.HasPrefix(text, "{") && strings.HasSuffix(text, "}") {
		var object map[string]any

		if err := json.Unmarshal([]byte(text), &object); err == nil {
			if _, named := object["host"]; !named {
				if _, named := object["hostname"]; !named {
					object["host"] = host
				}
			}

			dated := false

			for _, key := range timeKeys {
				if _, named := object[key]; named {
					dated = true

					break
				}
			}

			if !dated {
				object["ts"] = line.Time.UTC().Format(time.RFC3339Nano)
			}

			switch context := object["context"].(type) {
			case nil:
				object["context"] = map[string]any{"kubernetes": kubernetes}
			case map[string]any:
				if _, named := context["kubernetes"]; !named {
					context["kubernetes"] = kubernetes
				}
			}

			if encoded, err := json.Marshal(object); err == nil {
				return encoded
			}
		}
	}

	context := map[string]any{"kubernetes": kubernetes}
	if line.Stream != "" {
		context["stream"] = line.Stream
	}

	encoded, _ := json.Marshal(map[string]any{
		"message": line.Text,
		"ts":      line.Time.UTC().Format(time.RFC3339Nano),
		"host":    host,
		"context": context,
	})

	return encoded
}

// tailMultiline turns the configuration into what the follower uses.
func tailMultiline(setting *config.Multiline) (*tail.Multiline, error) {
	if setting == nil {
		return nil, nil
	}

	return tail.NewMultiline(setting.Mode, setting.Pattern, setting.MaxLines, setting.MaxBytes, time.Duration(setting.TimeoutMS)*time.Millisecond)
}

// runKubeMetrics reports the CPU and memory of the node's containers and pods.
func (a *Agent) runKubeMetrics(ctx context.Context) {
	settings := a.Config.Kubernetes.Metrics

	seconds := settings.IntervalSeconds
	if seconds <= 0 {
		seconds = config.DefaultMetricsInterval
	}

	interval := time.Duration(seconds) * time.Second
	collector := containermetrics.NewCollector(settings.CgroupRoot, settings.MaxSeries)

	read := func() []containermetrics.Container {
		running := kube.ListRunning(a.Config.Kubernetes.ContainersDir)
		containers := make([]containermetrics.Container, 0, len(running))

		for _, one := range running {
			if !a.Kube.Mapper.Accept(one.Namespace, one.Pod, one.Container) {
				continue
			}

			containers = append(containers, containermetrics.Container{
				Namespace: one.Namespace,
				Pod:       one.Pod,
				Name:      one.Container,
				ID:        one.ContainerID,
				Workload:  a.Kube.Registry.WorkloadOf(ctx, one.Namespace, one.Pod),
			})
		}

		return containers
	}

	// A first reading straight away, so the first interval has a CPU reading to compare with.
	collector.Collect(read(), a.Now())

	if a.Kube.Registry.API == nil {
		a.logf("kubernetes metrics: container and pod CPU and memory are reported without the workload name (no API)")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	warned := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := a.Now()
			points := collector.Collect(read(), now)

			if collector.Dropped > 0 && !warned {
				warned = true

				a.logf("kubernetes metrics: %d point(s) left out to keep to max_series %d; raise it, or exclude namespaces, if you want them", collector.Dropped, collector.MaxSeries)
			}

			converted := make([]hostmetrics.Point, 0, len(points))
			for _, point := range points {
				converted = append(converted, hostmetrics.Point{Name: point.Name, Value: point.Value, Labels: point.Labels})
			}

			deliver, cancel := context.WithTimeout(ctx, interval)

			for _, payload := range metricsPayloads(converted, a.Config.Host, now, pointsPerPost) {
				a.Sender.Post(deliver, metricsPath, payload)
			}

			cancel()
		}
	}
}

// Describe is a line for the log at start-up.
func (k *Kubernetes) Describe() string {
	api := "without the API (path only)"
	if k.Registry.API != nil {
		api = "with pod labels and owners from the API"
	}

	return fmt.Sprintf("kubernetes: node %s, %s/*/*/*.log, host from the %s, %s, %d route(s)", k.Config.NodeName, k.Config.PodsDir, k.Config.Host, api, len(k.Config.Routes))
}
