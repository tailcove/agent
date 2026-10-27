package kube

import (
	"context"
	"sync"
	"time"
)

// Registry answers "which pod is this log file" from the path, and adds what
// only the API knows (labels, the owner) when it can. A pod that is not in the
// list yet (it was created a moment ago) makes the registry ask again, but not
// more than once every MinRefresh: lines are never held back for the API, they
// go out with what the path says.
type Registry struct {
	PodsDir string
	Node    string
	// API is nil when the agent is not to ask the cluster, or cannot.
	API *API
	// Mapper decides what is kept of the labels.
	Mapper *Mapper
	// Interval is how often the whole list is read again; MinRefresh is how soon a
	// pod that is not known may make it be read earlier.
	Interval   time.Duration
	MinRefresh time.Duration
	Logf       func(format string, args ...any)
	Now        func() time.Time

	mu        sync.Mutex
	pods      map[string]PodInfo
	refreshed time.Time
	failing   bool
}

// Lookup is the meta of a log file, or false when the path is not a pod log.
func (r *Registry) Lookup(ctx context.Context, path string) (Meta, bool) {
	parsed, ok := ParsePodLog(r.PodsDir, path)
	if !ok {
		return Meta{}, false
	}

	meta := Meta{Namespace: parsed.Namespace, Pod: parsed.Pod, UID: parsed.UID, Container: parsed.Container, Restarts: parsed.Restarts, Node: r.Node}

	if r.API == nil {
		return meta, true
	}

	pod, known := r.pod(ctx, parsed.UID)
	if !known {
		return meta, true
	}

	meta.Enriched = true
	meta.Workload = pod.Workload
	meta.Labels = pod.Labels

	return meta, true
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

func (r *Registry) pod(ctx context.Context, uid string) (PodInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	interval, minimum := r.Interval, r.MinRefresh

	if interval <= 0 {
		interval = 30 * time.Second
	}

	if minimum <= 0 {
		minimum = 2 * time.Second
	}

	age := r.now().Sub(r.refreshed)
	pod, known := r.pods[uid]

	if age >= interval || (!known && age >= minimum) {
		r.refreshLocked(ctx)
		pod, known = r.pods[uid]
	}

	return pod, known
}

// WorkloadOf is the owner of a pod found by name, or "" when it is not known
// (no API, or the pod is not in the list). The metrics name pods this way: the
// container links carry no uid.
func (r *Registry) WorkloadOf(ctx context.Context, namespace, pod string) string {
	if r.API == nil {
		return ""
	}

	for _, info := range r.Pods(ctx) {
		if info.Namespace == namespace && info.Name == pod {
			return info.Workload
		}
	}

	return ""
}

// Pods is every pod the API lists on the node, read again when the list is old.
func (r *Registry) Pods(ctx context.Context) []PodInfo {
	if r.API == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	if r.now().Sub(r.refreshed) >= interval {
		r.refreshLocked(ctx)
	}

	pods := make([]PodInfo, 0, len(r.pods))
	for _, pod := range r.pods {
		pods = append(pods, pod)
	}

	return pods
}

func (r *Registry) refreshLocked(ctx context.Context) {
	r.refreshed = r.now()

	fetch, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	list, err := r.API.PodsOnNode(fetch, r.Node)
	if err != nil {
		// Said once per outage, not every refresh.
		if !r.failing && r.Logf != nil {
			r.Logf("kubernetes: could not list the pods on %s, lines carry what the file path says until it works: %v", r.Node, err)
		}

		r.failing = true

		return
	}

	if r.failing && r.Logf != nil {
		r.Logf("kubernetes: the pod list works again")
	}

	r.failing = false
	r.pods = make(map[string]PodInfo, len(list))

	for _, pod := range list {
		r.pods[pod.UID] = pod
	}
}
