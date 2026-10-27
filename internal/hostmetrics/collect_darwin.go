//go:build darwin

package hostmetrics

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// Point is one metric value, ready to send.
type Point struct {
	Name   string
	Value  float64
	Labels map[string]string
}

// Collector reads the Mac's figures by running the system's own tools (top,
// sysctl, vm_stat, netstat, mount), which are present on every macOS and need no
// privilege. A tool that fails or prints something unexpected costs only its
// own points.
type Collector struct {
	ExcludeIfaces []string
	ExcludeMounts []string

	// run executes a tool and returns its output; tests replace it.
	run func(name string, args ...string) (string, error)
	// now is the clock, for the uptime.
	now func() time.Time
}

// NewCollector reads from the real system.
func NewCollector(excludeIfaces, excludeMounts []string) *Collector {
	return &Collector{ExcludeIfaces: excludeIfaces, ExcludeMounts: excludeMounts, run: runTool, now: time.Now}
}

// NewCollectorAt reads from the real system: macOS has no /proc to point elsewhere.
func NewCollectorAt(_ string, excludeIfaces, excludeMounts []string) *Collector {
	return NewCollector(excludeIfaces, excludeMounts)
}

// DefaultExcludedInterfaces are virtual interfaces that would only add series.
var DefaultExcludedInterfaces = []string{"lo*", "gif*", "stf*", "utun*", "awdl*", "llw*", "anpi*", "bridge*", "ap*", "vmenet*", "XHC*"}

// runTool runs a tool with a time limit and returns what it printed.
func runTool(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, name, args...).Output()

	return string(output), err
}

// Collect returns the current points.
func (c *Collector) Collect() []Point {
	var points []Point

	if text, err := c.run("top", "-l", "2", "-n", "0", "-s", "1"); err == nil {
		if usage, ok := parseDarwinCPU(text); ok {
			points = append(points, Point{Name: "system.cpu.usage_percent", Value: usage})
		}
	}

	if text, err := c.run("sysctl", "-n", "vm.loadavg"); err == nil {
		if load, err := parseDarwinLoad(text); err == nil {
			points = append(points,
				Point{Name: "system.load.1m", Value: load.One},
				Point{Name: "system.load.5m", Value: load.Five},
				Point{Name: "system.load.15m", Value: load.Fifteen},
			)
		}
	}

	points = append(points, c.memory()...)

	if text, err := c.run("netstat", "-ibn"); err == nil {
		for _, iface := range parseDarwinNetstat(text) {
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
			)
		}
	}

	points = append(points, c.disks()...)

	if text, err := c.run("sysctl", "-n", "kern.boottime"); err == nil {
		if boot, ok := parseDarwinBoot(text); ok {
			if seconds := c.now().Unix() - boot; seconds > 0 {
				points = append(points, Point{Name: "system.uptime_seconds", Value: float64(seconds)})
			}
		}
	}

	return points
}

func (c *Collector) memory() []Point {
	var memory Memory

	if text, err := c.run("sysctl", "-n", "hw.memsize"); err == nil {
		if total, ok := parseSize(trimNewline(text)); ok {
			memory.Total = total
		}
	}

	if memory.Total == 0 {
		return nil
	}

	if text, err := c.run("vm_stat"); err == nil {
		if available, err := parseVMStat(text); err == nil {
			memory.Available = min(available, memory.Total)
		}
	}

	if text, err := c.run("sysctl", "-n", "vm.swapusage"); err == nil {
		if total, free, ok := parseDarwinSwap(text); ok {
			memory.SwapTotal, memory.SwapFree = total, free
		}
	}

	return memoryPoints(memory)
}

func (c *Collector) disks() []Point {
	text, err := c.run("mount")
	if err != nil {
		return nil
	}

	var points []Point

	for _, mount := range parseDarwinMounts(text) {
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

	return points
}

func trimNewline(text string) string {
	for len(text) > 0 && (text[len(text)-1] == '\n' || text[len(text)-1] == ' ') {
		text = text[:len(text)-1]
	}

	return text
}
