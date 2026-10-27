package hostmetrics

import (
	"fmt"
	"strconv"
	"strings"
)

// The readers below parse what macOS's own tools print (top, sysctl, vm_stat,
// netstat, mount). They have no build tag so that they are tested on every
// platform, from captured output, and only the collector that runs the tools is
// macOS-only.

// parseDarwinCPU reads the last "CPU usage: 3.2% user, 5.1% sys, 91.6% idle"
// line of `top -l 2`: the first sample is an average since boot, the last one
// covers the interval between the two. It returns the busy percentage.
func parseDarwinCPU(text string) (float64, bool) {
	busy, found := 0.0, false

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "CPU usage:") {
			continue
		}

		for _, part := range strings.Split(strings.TrimPrefix(line, "CPU usage:"), ",") {
			fields := strings.Fields(part)
			if len(fields) != 2 || fields[1] != "idle" {
				continue
			}

			idle, err := strconv.ParseFloat(strings.TrimSuffix(fields[0], "%"), 64)
			if err != nil || idle < 0 || idle > 100 {
				continue
			}

			busy, found = 100-idle, true
		}
	}

	return busy, found
}

// parseDarwinLoad reads `sysctl -n vm.loadavg`: "{ 1.52 1.60 1.65 }".
func parseDarwinLoad(text string) (Load, error) {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(text), "{}"))
	if len(fields) < 3 {
		return Load{}, fmt.Errorf("unexpected vm.loadavg %q", text)
	}

	var values [3]float64

	for i := range values {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return Load{}, fmt.Errorf("unexpected vm.loadavg %q", text)
		}

		values[i] = value
	}

	return Load{One: values[0], Five: values[1], Fifteen: values[2]}, nil
}

// parseVMStat reads `vm_stat` and returns the bytes that are available without
// swapping or compressing more: free, inactive, speculative and purgeable pages.
func parseVMStat(text string) (available uint64, err error) {
	pageSize := uint64(4096)
	pages := map[string]uint64{}

	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			if start := strings.Index(line, "page size of "); start >= 0 {
				if size, err := strconv.ParseUint(strings.Fields(line[start+len("page size of "):])[0], 10, 64); err == nil && size > 0 {
					pageSize = size
				}
			}

			continue
		}

		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}

		if count, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64); err == nil {
			pages[strings.TrimSpace(name)] = count
		}
	}

	if _, ok := pages["Pages free"]; !ok {
		return 0, fmt.Errorf("unexpected vm_stat output")
	}

	count := pages["Pages free"] + pages["Pages inactive"] + pages["Pages speculative"] + pages["Pages purgeable"]

	return count * pageSize, nil
}

// parseDarwinSwap reads `sysctl -n vm.swapusage`:
// "total = 2048.00M  used = 1056.75M  free = 991.25M  (encrypted)".
func parseDarwinSwap(text string) (total, free uint64, ok bool) {
	values := map[string]uint64{}
	fields := strings.Fields(text)

	for i := 0; i+2 < len(fields); i++ {
		if fields[i+1] != "=" {
			continue
		}

		if bytes, parsed := parseSize(fields[i+2]); parsed {
			values[fields[i]] = bytes
		}
	}

	total, haveTotal := values["total"]
	free, haveFree := values["free"]

	return total, free, haveTotal && haveFree
}

// parseSize reads "1056.75M" (K, M, G or T) as bytes.
func parseSize(text string) (uint64, bool) {
	if text == "" {
		return 0, false
	}

	multiplier := float64(1)

	switch text[len(text)-1] {
	case 'K':
		multiplier = 1 << 10
	case 'M':
		multiplier = 1 << 20
	case 'G':
		multiplier = 1 << 30
	case 'T':
		multiplier = 1 << 40
	}

	if multiplier != 1 {
		text = text[:len(text)-1]
	}

	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value < 0 {
		return 0, false
	}

	return uint64(value * multiplier), true
}

// parseDarwinBoot reads `sysctl -n kern.boottime`: "{ sec = 1761000000, usec = 12 } Mon Oct 20 ...".
func parseDarwinBoot(text string) (int64, bool) {
	const marker = "{ sec = "

	start := strings.Index(text, marker)
	if start < 0 {
		return 0, false
	}

	rest := text[start+len(marker):]
	end := strings.IndexAny(rest, ", }")

	if end < 0 {
		return 0, false
	}

	seconds, err := strconv.ParseInt(rest[:end], 10, 64)

	return seconds, err == nil && seconds > 0
}

// parseDarwinNetstat reads `netstat -ibn`: one row per link of each interface,
//
//	Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll
//
// where the address is missing on some interfaces, so the counters are read
// from the end of the row.
func parseDarwinNetstat(text string) []Interface {
	var interfaces []Interface

	seen := map[string]bool{}

	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !strings.HasPrefix(fields[2], "<Link#") || seen[fields[0]] {
			continue
		}

		counters := fields[len(fields)-7:]
		numbers := make([]uint64, len(counters))
		valid := true

		for i, counter := range counters {
			value, err := strconv.ParseUint(counter, 10, 64)
			if err != nil {
				valid = false

				break
			}

			numbers[i] = value
		}

		if !valid {
			continue
		}

		seen[fields[0]] = true
		interfaces = append(interfaces, Interface{
			Name:      strings.TrimSuffix(fields[0], "*"),
			RxPackets: numbers[0], RxErrors: numbers[1], RxBytes: numbers[2],
			TxPackets: numbers[3], TxErrors: numbers[4], TxBytes: numbers[5],
		})
	}

	return interfaces
}

// parseDarwinMounts reads `mount`: "/dev/disk3s1s1 on / (apfs, sealed, local, read-only)".
// Only disks are returned; the system's helper volumes (VM, Preboot, Update) are left out.
func parseDarwinMounts(text string) []Mount {
	var mounts []Mount

	for _, line := range strings.Split(text, "\n") {
		device, rest, found := strings.Cut(line, " on ")
		if !found || !strings.HasPrefix(device, "/dev/") {
			continue
		}

		path, options, found := strings.Cut(rest, " (")
		if !found {
			continue
		}

		if strings.HasPrefix(path, "/System/Volumes/") && path != "/System/Volumes/Data" {
			continue
		}

		fstype, _, _ := strings.Cut(options, ",")

		mounts = append(mounts, Mount{Device: device, Path: path, Type: strings.TrimSuffix(fstype, ")")})
	}

	return mounts
}
