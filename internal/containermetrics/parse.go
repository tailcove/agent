// Package containermetrics reads the CPU and memory of containers from the
// kernel's cgroup files on a node, and turns them into metric points. It needs
// no container runtime and no API: a container's cgroup is found by its id.
package containermetrics

import (
	"bufio"
	"strconv"
	"strings"
)

// noLimit is how cgroup v1 writes "no memory limit": a number near 2^63.
const noLimit = uint64(1) << 60

// Stats is one reading of one container.
type Stats struct {
	// CPUSeconds is the cumulative CPU time used.
	CPUSeconds float64
	// ThrottledSeconds is the cumulative time the container was held back by its CPU limit.
	ThrottledSeconds float64
	// CPULimitCores is the CPU limit in cores, 0 when there is none.
	CPULimitCores float64
	// MemoryUsage is what the cgroup counts as used, page cache included.
	MemoryUsage uint64
	// MemoryWorkingSet is usage less the cache the kernel can drop: what the
	// kubelet judges an eviction by.
	MemoryWorkingSet uint64
	// MemoryLimit is the memory limit in bytes, 0 when there is none.
	MemoryLimit uint64
	// OOMKills is the cumulative number of processes the kernel killed for memory.
	OOMKills uint64
}

// keyValues reads "name value" lines.
func keyValues(text string) map[string]uint64 {
	values := map[string]uint64{}
	scanner := bufio.NewScanner(strings.NewReader(text))

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}

		if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
			values[fields[0]] = value
		}
	}

	return values
}

// single reads a file that holds one number. "max" is no limit.
func single(text string) (value uint64, limited bool, ok bool) {
	text = strings.TrimSpace(text)

	if text == "max" {
		return 0, false, true
	}

	number, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false, false
	}

	return number, true, true
}

// ParseV2 builds Stats from the files of a cgroup v2 container directory.
// Files that are missing are passed as empty strings and leave their fields zero.
func ParseV2(cpuStat, cpuMax, memoryCurrent, memoryStat, memoryMax, memoryEvents string) Stats {
	var stats Stats

	cpu := keyValues(cpuStat)
	stats.CPUSeconds = float64(cpu["usage_usec"]) / 1e6
	stats.ThrottledSeconds = float64(cpu["throttled_usec"]) / 1e6

	if fields := strings.Fields(cpuMax); len(fields) == 2 && fields[0] != "max" {
		quota, errQuota := strconv.ParseUint(fields[0], 10, 64)
		period, errPeriod := strconv.ParseUint(fields[1], 10, 64)

		if errQuota == nil && errPeriod == nil && period > 0 {
			stats.CPULimitCores = float64(quota) / float64(period)
		}
	}

	if current, _, ok := single(memoryCurrent); ok {
		stats.MemoryUsage = current
		stats.MemoryWorkingSet = workingSet(current, keyValues(memoryStat)["inactive_file"])
	}

	if limit, limited, ok := single(memoryMax); ok && limited {
		stats.MemoryLimit = limit
	}

	stats.OOMKills = keyValues(memoryEvents)["oom_kill"]

	return stats
}

// ParseV1 builds Stats from the files of cgroup v1 (separate cpu and memory trees).
func ParseV1(cpuacctUsage, cpuStat, quota, period, memoryUsage, memoryStat, memoryLimit, oomControl string) Stats {
	var stats Stats

	if ns, _, ok := single(cpuacctUsage); ok {
		stats.CPUSeconds = float64(ns) / 1e9
	}

	stats.ThrottledSeconds = float64(keyValues(cpuStat)["throttled_time"]) / 1e9

	// A quota of -1 is no limit.
	if q, err := strconv.ParseInt(strings.TrimSpace(quota), 10, 64); err == nil && q > 0 {
		if p, err := strconv.ParseInt(strings.TrimSpace(period), 10, 64); err == nil && p > 0 {
			stats.CPULimitCores = float64(q) / float64(p)
		}
	}

	if usage, _, ok := single(memoryUsage); ok {
		stats.MemoryUsage = usage
		memory := keyValues(memoryStat)

		inactive := memory["total_inactive_file"]
		if inactive == 0 {
			inactive = memory["inactive_file"]
		}

		stats.MemoryWorkingSet = workingSet(usage, inactive)
	}

	if limit, limited, ok := single(memoryLimit); ok && limited && limit < noLimit {
		stats.MemoryLimit = limit
	}

	stats.OOMKills = keyValues(oomControl)["oom_kill"]

	return stats
}

// workingSet is usage without the inactive file cache, never below zero.
func workingSet(usage, inactiveFile uint64) uint64 {
	if inactiveFile >= usage {
		return 0
	}

	return usage - inactiveFile
}
