// Package hostmetrics reads CPU, memory, disk and network figures from the
// kernel's /proc files and turns them into metric points.
package hostmetrics

import (
	"bufio"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// CPUTimes is the cumulative time the CPUs have spent, in clock ticks.
type CPUTimes struct {
	Total uint64
	Idle  uint64
}

// parseCPUStat reads the aggregate "cpu " line of /proc/stat.
func parseCPUStat(text string) (CPUTimes, error) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)

		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}

		var total, idle uint64

		// user nice system idle iowait irq softirq steal (guest is already in user)
		for i, field := range fields[1:] {
			if i >= 8 {
				break
			}

			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return CPUTimes{}, fmt.Errorf("cpu time %q: %w", field, err)
			}

			total += value

			if i == 3 || i == 4 {
				idle += value
			}
		}

		return CPUTimes{Total: total, Idle: idle}, nil
	}

	return CPUTimes{}, fmt.Errorf("no cpu line in /proc/stat")
}

// cpuUsagePercent is the share of non-idle time between two readings.
func cpuUsagePercent(before, after CPUTimes) (float64, bool) {
	if after.Total <= before.Total || after.Idle < before.Idle {
		return 0, false
	}

	total := float64(after.Total - before.Total)
	idle := float64(after.Idle - before.Idle)

	return (total - idle) / total * 100, true
}

// Memory holds /proc/meminfo figures in bytes.
type Memory struct {
	Total, Available, SwapTotal, SwapFree uint64
}

func parseMeminfo(text string) (Memory, error) {
	var memory Memory

	found := 0

	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		name, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}

		kilobytes, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}

		bytes := kilobytes * 1024

		switch name {
		case "MemTotal":
			memory.Total = bytes
			found++
		case "MemAvailable":
			memory.Available = bytes
			found++
		case "SwapTotal":
			memory.SwapTotal = bytes
		case "SwapFree":
			memory.SwapFree = bytes
		}
	}

	if found < 2 {
		return Memory{}, fmt.Errorf("MemTotal or MemAvailable missing from /proc/meminfo")
	}

	return memory, nil
}

// Load holds the 1, 5 and 15 minute load averages.
type Load struct{ One, Five, Fifteen float64 }

func parseLoadavg(text string) (Load, error) {
	fields := strings.Fields(text)
	if len(fields) < 3 {
		return Load{}, fmt.Errorf("unexpected /proc/loadavg")
	}

	var values [3]float64

	for i := range values {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return Load{}, err
		}

		values[i] = value
	}

	return Load{values[0], values[1], values[2]}, nil
}

// Interface holds one network interface's cumulative counters.
type Interface struct {
	Name                                  string
	RxBytes, RxPackets, RxErrors, RxDrops uint64
	TxBytes, TxPackets, TxErrors, TxDrops uint64
}

func parseNetDev(text string) []Interface {
	var interfaces []Interface

	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) < 16 {
			continue
		}

		numbers := make([]uint64, 16)
		valid := true

		for i := range numbers {
			value, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				valid = false

				break
			}

			numbers[i] = value
		}

		if !valid {
			continue
		}

		interfaces = append(interfaces, Interface{
			Name:    strings.TrimSpace(name),
			RxBytes: numbers[0], RxPackets: numbers[1], RxErrors: numbers[2], RxDrops: numbers[3],
			TxBytes: numbers[8], TxPackets: numbers[9], TxErrors: numbers[10], TxDrops: numbers[11],
		})
	}

	return interfaces
}

// Mount is a mounted filesystem.
type Mount struct{ Device, Path, Type string }

// realFilesystems are the types that hold data on a disk (or a network share);
// everything else in /proc/mounts is the kernel's own bookkeeping.
var realFilesystems = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "jfs": true,
	"reiserfs": true, "nfs": true, "nfs4": true, "cifs": true, "ufs": true,
}

func parseMounts(text string) []Mount {
	var mounts []Mount

	seen := map[string]bool{}

	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !realFilesystems[fields[2]] {
			continue
		}

		path := unescapeMount(fields[1])
		if seen[path] {
			continue
		}

		seen[path] = true
		mounts = append(mounts, Mount{Device: fields[0], Path: path, Type: fields[2]})
	}

	return mounts
}

// unescapeMount undoes the octal escapes /proc/mounts uses for spaces and the like.
func unescapeMount(path string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

	return replacer.Replace(path)
}

// parseUptime reads the first field of /proc/uptime.
func parseUptime(text string) (float64, bool) {
	fields := strings.Fields(text)
	if len(fields) < 1 {
		return 0, false
	}

	seconds, err := strconv.ParseFloat(fields[0], 64)

	return seconds, err == nil
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
