//go:build darwin

package hostmetrics

import (
	"errors"
	"testing"
	"time"
)

func fakeTools(outputs map[string]string) func(string, ...string) (string, error) {
	return func(name string, args ...string) (string, error) {
		key := name
		for _, arg := range args {
			key += " " + arg
		}

		if output, ok := outputs[key]; ok {
			return output, nil
		}

		return "", errors.New("no such tool in the test: " + key)
	}
}

func TestCollectOnAMacReportsTheSameNamesAsOnLinux(t *testing.T) {
	collector := NewCollector(DefaultExcludedInterfaces, nil)
	collector.now = func() time.Time { return time.Unix(1761003600, 0) }
	collector.run = fakeTools(map[string]string{
		"top -l 2 -n 0 -s 1":      "CPU usage: 10.0% user, 5.0% sys, 85.0% idle\n",
		"sysctl -n vm.loadavg":    "{ 1.00 2.00 3.00 }\n",
		"sysctl -n hw.memsize":    "17179869184\n",
		"vm_stat":                 "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free: 100000.\nPages inactive: 100000.\n",
		"sysctl -n vm.swapusage":  "total = 1024.00M  used = 256.00M  free = 768.00M  (encrypted)\n",
		"netstat -ibn":            "Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll\nen0 1500 <Link#6> aa:bb:cc:dd:ee:ff 10 0 2000 20 0 3000 0\nlo0 16384 <Link#1> 5 0 500 5 0 500 0\n",
		"mount":                   "/dev/disk3s1s1 on / (apfs, local, journaled)\n",
		"sysctl -n kern.boottime": "{ sec = 1761000000, usec = 0 } Mon Oct 20 22:40:00 2025\n",
	})

	values := map[string]float64{}
	for _, point := range collector.Collect() {
		if len(point.Labels) == 0 {
			values[point.Name] = point.Value
		}
	}

	for name, want := range map[string]float64{
		"system.cpu.usage_percent": 15,
		"system.load.1m":           1,
		"system.mem.total_bytes":   17179869184,
		"system.uptime_seconds":    3600,
	} {
		if got := values[name]; got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	if _, ok := values["system.swap.used_percent"]; !ok {
		t.Error("no swap figure")
	}
}

func TestACollectorWithoutToolsReportsNothingInsteadOfFailing(t *testing.T) {
	collector := NewCollector(nil, nil)
	collector.run = func(string, ...string) (string, error) { return "", errors.New("missing") }

	if points := collector.Collect(); len(points) != 0 {
		t.Errorf("got %d points", len(points))
	}
}
