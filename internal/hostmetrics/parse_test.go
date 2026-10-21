package hostmetrics

import (
	"math"
	"testing"
)

func TestCPUUsageFromTwoReadings(t *testing.T) {
	before, err := parseCPUStat("cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 50 0 50 400 0 0 0 0 0 0\n")
	if err != nil {
		t.Fatal(err)
	}

	after, err := parseCPUStat("cpu  200 0 200 1000 0 0 0 0 0 0\n")
	if err != nil {
		t.Fatal(err)
	}

	usage, ok := cpuUsagePercent(before, after)

	// 400 ticks passed, 200 of them idle.
	if !ok || math.Abs(usage-50) > 1e-9 {
		t.Fatalf("usage = %v, %v", usage, ok)
	}
}

func TestCPUUsageIsNotReportedWhenCountersWentBackwardsOrStood(t *testing.T) {
	if _, ok := cpuUsagePercent(CPUTimes{Total: 100, Idle: 50}, CPUTimes{Total: 100, Idle: 50}); ok {
		t.Error("a reading with no elapsed time has no usage")
	}

	if _, ok := cpuUsagePercent(CPUTimes{Total: 100, Idle: 50}, CPUTimes{Total: 90, Idle: 40}); ok {
		t.Error("counters that went backwards (a reboot) have no usage")
	}
}

func TestParseCPUStatRefusesWhatIsNotAKernelLine(t *testing.T) {
	for _, text := range []string{"", "intr 1 2 3\n", "cpu  a b c d e\n"} {
		if _, err := parseCPUStat(text); err == nil {
			t.Errorf("%q should not parse", text)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	memory, err := parseMeminfo("MemTotal:       16384000 kB\nMemFree:         1000 kB\nMemAvailable:    8192000 kB\nSwapTotal:       2048000 kB\nSwapFree:        1024000 kB\n")
	if err != nil {
		t.Fatal(err)
	}

	if memory.Total != 16384000*1024 || memory.Available != 8192000*1024 || memory.SwapTotal != 2048000*1024 || memory.SwapFree != 1024000*1024 {
		t.Fatalf("%+v", memory)
	}

	if _, err := parseMeminfo("MemTotal: 1 kB\n"); err == nil {
		t.Error("a meminfo without MemAvailable should not parse")
	}
}

func TestMemoryPointsIncludeSwapOnlyWhenThereIsSwap(t *testing.T) {
	withSwap := memoryPoints(Memory{Total: 1000, Available: 250, SwapTotal: 100, SwapFree: 50})

	got := map[string]float64{}
	for _, point := range withSwap {
		got[point.Name] = point.Value
	}

	if got["system.mem.used_percent"] != 75 || got["system.mem.used_bytes"] != 750 || got["system.swap.used_percent"] != 50 {
		t.Fatalf("%v", got)
	}

	for _, point := range memoryPoints(Memory{Total: 1000, Available: 250}) {
		if point.Name == "system.swap.used_percent" {
			t.Error("no swap, no swap figure")
		}
	}
}

func TestParseLoadavg(t *testing.T) {
	load, err := parseLoadavg("0.52 0.40 0.31 2/418 12345\n")
	if err != nil || load.One != 0.52 || load.Five != 0.40 || load.Fifteen != 0.31 {
		t.Fatalf("%+v %v", load, err)
	}

	if _, err := parseLoadavg("x y z"); err == nil {
		t.Error("garbage should not parse")
	}
}

func TestParseNetDev(t *testing.T) {
	text := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0
  eth0: 5000 50 1 2 0 0 0 0 7000 70 3 4 0 0 0 0
`

	interfaces := parseNetDev(text)
	if len(interfaces) != 2 {
		t.Fatalf("%+v", interfaces)
	}

	eth := interfaces[1]
	if eth.Name != "eth0" || eth.RxBytes != 5000 || eth.TxBytes != 7000 || eth.RxErrors != 1 || eth.TxErrors != 3 || eth.RxDrops != 2 || eth.TxDrops != 4 {
		t.Fatalf("%+v", eth)
	}
}

func TestParseMountsKeepsOnlyRealFilesystemsOnce(t *testing.T) {
	text := `sysfs /sys sysfs rw 0 0
proc /proc proc rw 0 0
/dev/sda1 / ext4 rw 0 0
/dev/sda1 / ext4 rw 0 0
tmpfs /run tmpfs rw 0 0
overlay /var/lib/docker/overlay2/x overlay rw 0 0
/dev/sdb1 /mnt/with\040space xfs rw 0 0
server:/share /data nfs4 rw 0 0
`

	mounts := parseMounts(text)
	if len(mounts) != 3 || mounts[0].Path != "/" || mounts[1].Path != "/mnt/with space" || mounts[2].Type != "nfs4" {
		t.Fatalf("%+v", mounts)
	}
}

func TestParseUptime(t *testing.T) {
	if seconds, ok := parseUptime("12345.67 54321.00\n"); !ok || seconds != 12345.67 {
		t.Fatalf("%v %v", seconds, ok)
	}

	if _, ok := parseUptime(""); ok {
		t.Error("empty uptime should not parse")
	}
}

func TestExcludePatterns(t *testing.T) {
	if !matchesAny(DefaultExcludedInterfaces, "veth1a2b3c") || !matchesAny(DefaultExcludedInterfaces, "lo") || matchesAny(DefaultExcludedInterfaces, "eth0") {
		t.Error("default interface exclusions are wrong")
	}
}
