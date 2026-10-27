package containermetrics

import (
	"sort"
	"time"
)

// Container is one container the node runs: where it belongs and its runtime id.
type Container struct {
	Namespace string
	Pod       string
	Name      string
	ID        string
	Workload  string
}

// Point is one metric value, ready to send.
type Point struct {
	Name   string
	Value  float64
	Labels map[string]string
}

// DefaultMaxSeries bounds the points of one reading. Every container adds
// several series and every pod two, and a source's plan has a series limit, so
// the agent keeps to a number of its own instead of leaving that to the server.
const DefaultMaxSeries = 500

// Collector reads the containers it is given and keeps the previous reading of
// each, to turn cumulative CPU time into cores.
type Collector struct {
	Locator   *Locator
	MaxSeries int

	last map[string]sample
	// Dropped is how many points the last Collect left out for MaxSeries.
	Dropped int
}

type sample struct {
	cpuSeconds float64
	at         time.Time
}

// NewCollector reads cgroups under root.
func NewCollector(root string, maxSeries int) *Collector {
	if maxSeries <= 0 {
		maxSeries = DefaultMaxSeries
	}

	return &Collector{Locator: NewLocator(root), MaxSeries: maxSeries, last: map[string]sample{}}
}

type podTotals struct {
	namespace, pod, workload string
	cores                    float64
	haveCores                bool
	workingSet               uint64
	haveMemory               bool
}

// Collect reads every container and returns the points: a pod's CPU and memory
// first (they are what is looked at most), then each container's. A container
// that has no cgroup (it is not running) adds nothing. The first reading of a
// container has no CPU usage in cores yet: there is nothing to compare with.
func (c *Collector) Collect(containers []Container, now time.Time) []Point {
	sort.Slice(containers, func(i, j int) bool {
		a, b := containers[i], containers[j]

		return a.Namespace+"/"+a.Pod+"/"+a.Name < b.Namespace+"/"+b.Pod+"/"+b.Name
	})

	seen := map[string]bool{}
	pods := map[string]*podTotals{}
	podOrder := []string{}
	var containerPoints []Point

	for _, container := range containers {
		dirs, found := c.Locator.Find(container.ID)
		if !found {
			continue
		}

		stats := c.Locator.Read(dirs)
		labels := labelsOf(container)
		seen[container.ID] = true

		key := container.Namespace + "/" + container.Pod

		total, known := pods[key]
		if !known {
			total = &podTotals{namespace: container.Namespace, pod: container.Pod, workload: container.Workload}
			pods[key] = total
			podOrder = append(podOrder, key)
		}

		total.workingSet += stats.MemoryWorkingSet
		total.haveMemory = true

		containerPoints = append(containerPoints,
			Point{"container.cpu.usage_seconds_total", stats.CPUSeconds, labels},
			Point{"container.cpu.throttled_seconds_total", stats.ThrottledSeconds, labels},
			Point{"container.memory.usage_bytes", float64(stats.MemoryUsage), labels},
			Point{"container.memory.working_set_bytes", float64(stats.MemoryWorkingSet), labels},
			Point{"container.memory.oom_kills_total", float64(stats.OOMKills), labels},
		)

		if previous, had := c.last[container.ID]; had {
			if cores, ok := coresBetween(previous, sample{stats.CPUSeconds, now}); ok {
				containerPoints = append(containerPoints, Point{"container.cpu.usage_cores", cores, labels})
				total.cores += cores
				total.haveCores = true
			}
		}

		if stats.CPULimitCores > 0 {
			containerPoints = append(containerPoints, Point{"container.cpu.limit_cores", stats.CPULimitCores, labels})
		}

		if stats.MemoryLimit > 0 {
			containerPoints = append(containerPoints, Point{"container.memory.limit_bytes", float64(stats.MemoryLimit), labels})
		}

		c.last[container.ID] = sample{stats.CPUSeconds, now}
	}

	// Forget containers that are gone, or the map grows with every pod that ever ran.
	for id := range c.last {
		if !seen[id] {
			delete(c.last, id)
		}
	}

	var points []Point

	for _, key := range podOrder {
		total := pods[key]
		labels := map[string]string{"namespace": total.namespace, "pod": total.pod}

		if total.workload != "" {
			labels["workload"] = total.workload
		}

		if total.haveCores {
			points = append(points, Point{"pod.cpu.usage_cores", total.cores, labels})
		}

		if total.haveMemory {
			points = append(points, Point{"pod.memory.working_set_bytes", float64(total.workingSet), labels})
		}
	}

	points = append(points, containerPoints...)

	c.Dropped = 0

	if len(points) > c.MaxSeries {
		c.Dropped = len(points) - c.MaxSeries
		points = points[:c.MaxSeries]
	}

	return points
}

func labelsOf(container Container) map[string]string {
	labels := map[string]string{"namespace": container.Namespace, "pod": container.Pod, "container": container.Name}

	if container.Workload != "" {
		labels["workload"] = container.Workload
	}

	return labels
}

// coresBetween is the CPU time used per second of wall time between two
// readings. A reading that went backwards (the container restarted under the
// same id is not possible, but a counter reset is) or a zero interval gives none.
func coresBetween(before, after sample) (float64, bool) {
	elapsed := after.at.Sub(before.at).Seconds()

	if elapsed <= 0 || after.cpuSeconds < before.cpuSeconds {
		return 0, false
	}

	return (after.cpuSeconds - before.cpuSeconds) / elapsed, true
}
