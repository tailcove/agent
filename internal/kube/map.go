package kube

import (
	"path"
	"sort"
	"strings"
)

// Meta is what is known about the pod and container a log line came from.
type Meta struct {
	Namespace string
	Pod       string
	UID       string
	Container string
	Restarts  int
	Node      string
	// Labels are the pod's labels the agent was asked to keep (see Mapper.LabelKeys).
	Labels map[string]string
	// Workload is what owns the pod: the Deployment, StatefulSet, DaemonSet or Job
	// (a ReplicaSet is followed up to its Deployment by name).
	Workload string
	// Enriched is true when the pod was found through the Kubernetes API, so the
	// labels and workload are known; false when only the path was.
	Enriched bool
}

// Route sends the lines of the pods it matches to another source, by that
// source's token. A route matches when the namespace matches Namespace (a
// glob, empty for any) and the pod carries every one of Labels.
type Route struct {
	Namespace string
	Labels    map[string]string
	Token     string
}

// Mapper decides which containers are followed, and how their lines are named
// and where they go.
type Mapper struct {
	// Host is what the line's host is: "pod" (the default), "node", "workload"
	// or "label:<key>".
	Host string
	// LabelKeys are the pod labels kept in a line's context.
	LabelKeys []string

	IncludeNamespaces []string
	ExcludeNamespaces []string
	ExcludePods       []string
	ExcludeContainers []string

	Routes []Route
}

// MaxLabels and MaxLabelValue bound what a pod can add to a line's context.
const (
	MaxLabels     = 10
	MaxLabelValue = 128
)

// Accept says whether a container's log is followed. Excluding wins over
// including; an empty include list means every namespace.
func (m *Mapper) Accept(namespace, pod, container string) bool {
	if matchAny(m.ExcludeNamespaces, namespace) || matchAny(m.ExcludePods, pod) || matchAny(m.ExcludeContainers, container) {
		return false
	}

	return len(m.IncludeNamespaces) == 0 || matchAny(m.IncludeNamespaces, namespace)
}

// HostFor is the host name for a pod's lines.
func (m *Mapper) HostFor(meta Meta) string {
	switch {
	case m.Host == "node" && meta.Node != "":
		return meta.Node
	case m.Host == "workload" && meta.Workload != "":
		return meta.Workload
	case strings.HasPrefix(m.Host, "label:"):
		if value := meta.Labels[strings.TrimPrefix(m.Host, "label:")]; value != "" {
			return value
		}
	}

	return meta.Pod
}

// TokenFor is the token of the first route that matches the pod, or "" when
// none does (the default source).
func (m *Mapper) TokenFor(meta Meta) string {
	for _, route := range m.Routes {
		if route.Namespace != "" && !matchOne(route.Namespace, meta.Namespace) {
			continue
		}

		matched := true

		for key, want := range route.Labels {
			if got, have := meta.Labels[key]; !have || got != want {
				matched = false

				break
			}
		}

		if matched {
			return route.Token
		}
	}

	return ""
}

// Context is the "kubernetes" object added to a line's context.
func (m *Mapper) Context(meta Meta) map[string]any {
	object := map[string]any{
		"namespace": meta.Namespace,
		"pod":       meta.Pod,
		"container": meta.Container,
		"restarts":  meta.Restarts,
	}

	if meta.Node != "" {
		object["node"] = meta.Node
	}

	if meta.Workload != "" {
		object["workload"] = meta.Workload
	}

	if labels := m.KeptLabels(meta.Labels); len(labels) > 0 {
		object["labels"] = labels
	}

	return object
}

// KeptLabels are the labels the agent was asked to keep, bounded in number and
// length: a pod's labels are written by whoever deploys it.
func (m *Mapper) KeptLabels(all map[string]string) map[string]string {
	keys := append([]string(nil), m.LabelKeys...)
	sort.Strings(keys)

	kept := map[string]string{}

	for _, key := range keys {
		if len(kept) >= MaxLabels {
			break
		}

		if value, found := all[key]; found {
			kept[key] = truncate(value, MaxLabelValue)
		}
	}

	return kept
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}

	cut := limit

	for cut > 0 && !isRuneStart(value[cut]) {
		cut--
	}

	return value[:cut]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func matchAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matchOne(pattern, name) {
			return true
		}
	}

	return false
}

func matchOne(pattern, name string) bool {
	matched, err := path.Match(pattern, name)

	return err == nil && matched
}
