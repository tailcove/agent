//go:build linux

package hostmetrics

import (
	"os"
	"path/filepath"
	"syscall"
)

// Point is one metric value, ready to send.
type Point struct {
	Name   string
	Value  float64
	Labels map[string]string
}

// Collector reads the host's figures. It remembers the previous CPU reading,
// so the first Collect has no CPU usage to report.
type Collector struct {
	ExcludeIfaces []string
	ExcludeMounts []string

	root     string
	lastCPU  CPUTimes
	haveLast bool
}

// NewCollector reads from the real /proc.
func NewCollector(excludeIfaces, excludeMounts []string) *Collector {
	return NewCollectorAt("/proc", excludeIfaces, excludeMounts)
}

// NewCollectorAt reads from the /proc mounted at root: the host's, mounted
// into a container as /host/proc, rather than the container's own view.
func NewCollectorAt(root string, excludeIfaces, excludeMounts []string) *Collector {
	if root == "" {
		root = "/proc"
	}

	return &Collector{ExcludeIfaces: excludeIfaces, ExcludeMounts: excludeMounts, root: root}
}

// DefaultExcludedInterfaces are virtual interfaces that would only add series.
var DefaultExcludedInterfaces = []string{"lo", "veth*", "docker*", "br-*", "cali*", "flannel*", "cni*", "virbr*"}

// Collect returns the current points. A source that cannot be read is skipped,
// so a host without swap or a restricted /proc still reports the rest.
func (c *Collector) Collect() []Point {
	var points []Point

	if text, err := os.ReadFile(filepath.Join(c.root, "stat")); err == nil {
		if times, err := parseCPUStat(string(text)); err == nil {
			if c.haveLast {
				if usage, ok := cpuUsagePercent(c.lastCPU, times); ok {
					points = append(points, Point{Name: "system.cpu.usage_percent", Value: usage})
				}
			}

			c.lastCPU, c.haveLast = times, true
		}
	}

	if text, err := os.ReadFile(filepath.Join(c.root, "loadavg")); err == nil {
		if load, err := parseLoadavg(string(text)); err == nil {
			points = append(points,
				Point{Name: "system.load.1m", Value: load.One},
				Point{Name: "system.load.5m", Value: load.Five},
				Point{Name: "system.load.15m", Value: load.Fifteen},
			)
		}
	}

	if text, err := os.ReadFile(filepath.Join(c.root, "meminfo")); err == nil {
		if memory, err := parseMeminfo(string(text)); err == nil {
			points = append(points, memoryPoints(memory)...)
		}
	}

	if text, err := os.ReadFile(filepath.Join(c.root, "net", "dev")); err == nil {
		for _, iface := range parseNetDev(string(text)) {
			if matchesAny(c.ExcludeIfaces, iface.Name) {
				continue
			}

			labels := map[string]string{"interface": iface.Name}
			points = append(points,
				Point{"system.net.bytes_recv", float64(iface.RxBytes), labels},
				Point{"system.net.bytes_sent", float64(iface.TxBytes), labels},
				Point{"system.net.packets_recv", float64(iface.RxPackets), labels},
				Point{"system.net.packets_sent", float64(iface.TxPackets), labels},
				Point{"system.net.errors", float64(iface.RxErrors + iface.TxErrors), labels},
				Point{"system.net.drops", float64(iface.RxDrops + iface.TxDrops), labels},
			)
		}
	}

	if text, err := os.ReadFile(filepath.Join(c.root, "mounts")); err == nil {
		for _, mount := range parseMounts(string(text)) {
			if matchesAny(c.ExcludeMounts, mount.Path) {
				continue
			}

			var stat syscall.Statfs_t
			if err := syscall.Statfs(mount.Path, &stat); err != nil || stat.Blocks == 0 {
				continue
			}

			size := uint64(stat.Bsize)
			total, free := stat.Blocks*size, stat.Bavail*size
			used := total - stat.Bfree*size
			labels := map[string]string{"mount": mount.Path, "fstype": mount.Type}

			points = append(points,
				Point{"system.disk.total_bytes", float64(total), labels},
				Point{"system.disk.used_bytes", float64(used), labels},
				Point{"system.disk.free_bytes", float64(free), labels},
				Point{"system.disk.used_percent", float64(used) / float64(used+free) * 100, labels},
			)
		}
	}

	if text, err := os.ReadFile(filepath.Join(c.root, "uptime")); err == nil {
		if seconds, ok := parseUptime(string(text)); ok {
			points = append(points, Point{Name: "system.uptime_seconds", Value: seconds})
		}
	}

	return points
}

func memoryPoints(memory Memory) []Point {
	used := memory.Total - min(memory.Available, memory.Total)

	points := []Point{
		{Name: "system.mem.total_bytes", Value: float64(memory.Total)},
		{Name: "system.mem.available_bytes", Value: float64(memory.Available)},
		{Name: "system.mem.used_bytes", Value: float64(used)},
	}

	if memory.Total > 0 {
		points = append(points, Point{Name: "system.mem.used_percent", Value: float64(used) / float64(memory.Total) * 100})
	}

	if memory.SwapTotal > 0 {
		swapUsed := memory.SwapTotal - min(memory.SwapFree, memory.SwapTotal)
		points = append(points, Point{Name: "system.swap.used_percent", Value: float64(swapUsed) / float64(memory.SwapTotal) * 100})
	}

	return points
}

func matchesAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if ok, err := filepath.Match(pattern, name); err == nil && ok {
			return true
		}
	}

	return false
}
