//go:build linux

package hostmetrics

import (
	"os"
	"testing"
)

func TestCollectReadsTheRealMachine(t *testing.T) {
	if _, err := os.Stat("/proc/meminfo"); err != nil {
		t.Skip("no /proc on this machine")
	}

	collector := NewCollector(DefaultExcludedInterfaces, nil)

	first := collector.Collect()
	second := collector.Collect()

	names := func(points []Point) map[string]bool {
		set := map[string]bool{}
		for _, point := range points {
			set[point.Name] = true
		}

		return set
	}

	if names(first)["system.cpu.usage_percent"] {
		t.Error("the first reading has nothing to compare with, so no CPU usage yet")
	}

	for _, name := range []string{"system.load.1m", "system.mem.used_percent", "system.mem.total_bytes", "system.uptime_seconds"} {
		if !names(second)[name] {
			t.Errorf("missing %s in %v", name, names(second))
		}
	}

	for _, point := range second {
		if point.Name == "system.mem.used_percent" && (point.Value < 0 || point.Value > 100) {
			t.Errorf("memory used_percent out of range: %v", point.Value)
		}
	}
}
