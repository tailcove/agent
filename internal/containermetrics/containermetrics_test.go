package containermetrics

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParseV2(t *testing.T) {
	stats := ParseV2(
		"usage_usec 2500000\nuser_usec 1\nnr_throttled 3\nthrottled_usec 400000\n",
		"50000 100000\n",
		"104857600\n",
		"anon 1\ninactive_file 20971520\nactive_file 5\n",
		"209715200\n",
		"low 0\nhigh 0\nmax 0\noom 2\noom_kill 1\n",
	)

	if !near(stats.CPUSeconds, 2.5) || !near(stats.ThrottledSeconds, 0.4) || !near(stats.CPULimitCores, 0.5) {
		t.Errorf("cpu: %+v", stats)
	}

	if stats.MemoryUsage != 104857600 || stats.MemoryWorkingSet != 104857600-20971520 || stats.MemoryLimit != 209715200 || stats.OOMKills != 1 {
		t.Errorf("memory: %+v", stats)
	}
}

func TestParseV2WithoutLimitsOrFiles(t *testing.T) {
	stats := ParseV2("usage_usec 10\n", "max 100000\n", "4096\n", "", "max\n", "")

	if stats.CPULimitCores != 0 || stats.MemoryLimit != 0 || stats.MemoryWorkingSet != 4096 {
		t.Errorf("got %+v", stats)
	}

	empty := ParseV2("", "", "", "", "", "")

	if (empty != Stats{}) {
		t.Errorf("missing files must read as zero, got %+v", empty)
	}
}

func TestParseV1(t *testing.T) {
	stats := ParseV1(
		"3000000000\n", "nr_periods 5\nthrottled_time 500000000\n", "200000\n", "100000\n",
		"1000\n", "total_inactive_file 400\ncache 9\n", "2000\n", "oom_kill_disable 0\nunder_oom 0\noom_kill 3\n",
	)

	if !near(stats.CPUSeconds, 3) || !near(stats.ThrottledSeconds, 0.5) || !near(stats.CPULimitCores, 2) {
		t.Errorf("cpu: %+v", stats)
	}

	if stats.MemoryUsage != 1000 || stats.MemoryWorkingSet != 600 || stats.MemoryLimit != 2000 || stats.OOMKills != 3 {
		t.Errorf("memory: %+v", stats)
	}

	free := ParseV1("1\n", "", "-1\n", "100000\n", "10\n", "", "9223372036854771712\n", "")

	if free.CPULimitCores != 0 || free.MemoryLimit != 0 {
		t.Errorf("v1 writes no limit as -1 and a number near 2^63: %+v", free)
	}
}

func TestTheWorkingSetNeverGoesBelowZero(t *testing.T) {
	if workingSet(100, 500) != 0 || workingSet(500, 100) != 400 {
		t.Error("working set arithmetic")
	}
}

func write(t *testing.T, dir, name, text string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// v2Container writes a container's cgroup the way the systemd driver lays it out.
func v2Container(t *testing.T, root, id string, usageUsec int, memory int) string {
	t.Helper()

	write(t, root, "cgroup.controllers", "cpu memory\n")

	dir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod0a1b.slice", "cri-containerd-"+id+".scope")
	write(t, dir, "cpu.stat", "usage_usec "+formatInt(usageUsec)+"\nthrottled_usec 0\n")
	write(t, dir, "cpu.max", "max 100000\n")
	write(t, dir, "memory.current", formatInt(memory)+"\n")
	write(t, dir, "memory.stat", "inactive_file 0\n")
	write(t, dir, "memory.max", "max\n")
	write(t, dir, "memory.events", "oom_kill 0\n")

	return dir
}

func formatInt(n int) string {
	if n == 0 {
		return "0"
	}

	digits := ""

	for ; n > 0; n /= 10 {
		digits = string(rune('0'+n%10)) + digits
	}

	return digits
}

func TestTheLocatorFindsAContainerByItsIDWhateverTheDriver(t *testing.T) {
	root := t.TempDir()
	systemd := v2Container(t, root, idA, 1, 1)

	// The cgroupfs driver names the directory by the bare id.
	bare := filepath.Join(root, "kubepods", "burstable", "pod0a1b", idB)
	write(t, bare, "cpu.stat", "usage_usec 1\n")

	locator := NewLocator(root)

	if locator.Version != V2 {
		t.Fatalf("version %v", locator.Version)
	}

	if dirs, ok := locator.Find(idA); !ok || dirs.CPU != systemd {
		t.Errorf("systemd layout: got %+v %v", dirs, ok)
	}

	if dirs, ok := locator.Find(idB); !ok || dirs.CPU != bare {
		t.Errorf("cgroupfs layout: got %+v %v", dirs, ok)
	}

	if _, ok := locator.Find(strings.Repeat("c", 64)); ok {
		t.Error("a container with no cgroup was found")
	}
}

func TestTheLocatorOnlyAcceptsAContainerID(t *testing.T) {
	root := t.TempDir()
	v2Container(t, root, idA, 1, 1)
	locator := NewLocator(root)

	for _, bad := range []string{"", "..", "../..", "kubepods.slice", "aaaa", strings.ToUpper(idA), idA + "/../.."} {
		if _, ok := locator.Find(bad); ok {
			t.Errorf("%q was accepted as a container id", bad)
		}
	}
}

func TestTheLocatorNoticesAContainerThatWentAway(t *testing.T) {
	root := t.TempDir()
	dir := v2Container(t, root, idA, 1, 1)
	locator := NewLocator(root)

	if _, ok := locator.Find(idA); !ok {
		t.Fatal("not found")
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	if _, ok := locator.Find(idA); ok {
		t.Error("a removed cgroup is still reported")
	}
}

func TestTheLocatorOnCgroupV1NeedsBothTrees(t *testing.T) {
	root := t.TempDir()
	cpu := filepath.Join(root, "cpu,cpuacct", "kubepods", "burstable", "pod1", idA)
	memory := filepath.Join(root, "memory", "kubepods", "burstable", "pod1", idA)
	write(t, cpu, "cpuacct.usage", "2000000000\n")
	write(t, cpu, "cpu.cfs_quota_us", "-1\n")
	write(t, cpu, "cpu.cfs_period_us", "100000\n")
	write(t, memory, "memory.usage_in_bytes", "5000\n")
	write(t, memory, "memory.stat", "total_inactive_file 1000\n")
	write(t, memory, "memory.limit_in_bytes", "9223372036854771712\n")

	locator := NewLocator(root)

	if locator.Version != V1 {
		t.Fatalf("version %v", locator.Version)
	}

	dirs, ok := locator.Find(idA)
	if !ok || dirs.CPU != cpu || dirs.Memory != memory {
		t.Fatalf("got %+v %v", dirs, ok)
	}

	stats := locator.Read(dirs)

	if !near(stats.CPUSeconds, 2) || stats.MemoryWorkingSet != 4000 {
		t.Errorf("got %+v", stats)
	}

	onlyCPU := t.TempDir()
	write(t, filepath.Join(onlyCPU, "cpu,cpuacct", "kubepods", idB), "cpuacct.usage", "1\n")

	if _, ok := NewLocator(onlyCPU).Find(idB); ok {
		t.Error("a container found in one tree only was reported")
	}
}

func TestTheLocatorDoesNotWalkDeeperThanTheLimit(t *testing.T) {
	root := t.TempDir()
	write(t, root, "cgroup.controllers", "cpu\n")

	deep := filepath.Join(root, "kubepods.slice", "a", "b", "c", "d", "e", "f", "g", "h", idA)
	write(t, deep, "cpu.stat", "usage_usec 1\n")

	if _, ok := NewLocator(root).Find(idA); ok {
		t.Error("walked past the depth limit")
	}
}

func container(name, id string) Container {
	return Container{Namespace: "shop", Pod: "cart-1", Name: name, ID: id, Workload: "cart"}
}

func find(points []Point, name string, labels map[string]string) (Point, bool) {
	for _, point := range points {
		if point.Name != name {
			continue
		}

		matches := true

		for key, value := range labels {
			if point.Labels[key] != value {
				matches = false
			}
		}

		if matches {
			return point, true
		}
	}

	return Point{}, false
}

func TestTheFirstReadingHasNoCoresAndTheSecondHas(t *testing.T) {
	root := t.TempDir()
	dir := v2Container(t, root, idA, 1_000_000, 1<<20)
	collector := NewCollector(root, 0)
	start := time.Unix(1_700_000_000, 0)

	first := collector.Collect([]Container{container("app", idA)}, start)

	if _, ok := find(first, "container.cpu.usage_cores", nil); ok {
		t.Error("the first reading has nothing to compare with")
	}

	if point, ok := find(first, "container.cpu.usage_seconds_total", nil); !ok || !near(point.Value, 1) {
		t.Errorf("the cumulative CPU time is reported at once: %+v", point)
	}

	write(t, dir, "cpu.stat", "usage_usec 4000000\nthrottled_usec 0\n") // 3 s of CPU in 10 s

	second := collector.Collect([]Container{container("app", idA)}, start.Add(10*time.Second))

	if point, ok := find(second, "container.cpu.usage_cores", map[string]string{"container": "app"}); !ok || !near(point.Value, 0.3) {
		t.Errorf("cores: %+v", point)
	}

	if point, ok := find(second, "pod.cpu.usage_cores", map[string]string{"pod": "cart-1"}); !ok || !near(point.Value, 0.3) {
		t.Errorf("pod cores: %+v", point)
	}
}

func TestACounterThatWentBackwardsGivesNoCoresInsteadOfANegativeNumber(t *testing.T) {
	root := t.TempDir()
	dir := v2Container(t, root, idA, 9_000_000, 1)
	collector := NewCollector(root, 0)
	start := time.Unix(1_700_000_000, 0)

	collector.Collect([]Container{container("app", idA)}, start)
	write(t, dir, "cpu.stat", "usage_usec 1000\n")

	points := collector.Collect([]Container{container("app", idA)}, start.Add(10*time.Second))

	for _, point := range points {
		if strings.HasSuffix(point.Name, "cores") && point.Name != "container.cpu.limit_cores" {
			t.Errorf("a reset produced %+v", point)
		}

		if point.Value < 0 {
			t.Errorf("a negative value: %+v", point)
		}
	}
}

func TestPodFiguresAddTheirContainersUp(t *testing.T) {
	root := t.TempDir()
	v2Container(t, root, idA, 1_000_000, 100)
	v2Container(t, root, idB, 1_000_000, 300)
	collector := NewCollector(root, 0)

	points := collector.Collect([]Container{container("app", idA), container("sidecar", idB)}, time.Unix(1_700_000_000, 0))

	if point, ok := find(points, "pod.memory.working_set_bytes", map[string]string{"namespace": "shop", "pod": "cart-1", "workload": "cart"}); !ok || point.Value != 400 {
		t.Errorf("pod memory: %+v %v", point, ok)
	}

	if _, ok := find(points, "container.memory.working_set_bytes", map[string]string{"container": "sidecar"}); !ok {
		t.Error("a container's own memory is reported too")
	}

	if points[0].Name != "pod.memory.working_set_bytes" {
		t.Errorf("pod figures come first, got %s", points[0].Name)
	}
}

func TestAContainerWithNoCgroupAddsNothingAndIsForgotten(t *testing.T) {
	root := t.TempDir()
	dir := v2Container(t, root, idA, 1_000_000, 100)
	collector := NewCollector(root, 0)
	now := time.Unix(1_700_000_000, 0)

	collector.Collect([]Container{container("app", idA)}, now)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	if points := collector.Collect([]Container{container("app", idA)}, now.Add(time.Minute)); len(points) != 0 {
		t.Errorf("got %d points for a container that is gone", len(points))
	}

	if len(collector.last) != 0 {
		t.Errorf("the collector remembers %d containers that are gone", len(collector.last))
	}
}

func TestLimitsAreReportedOnlyWhenThereAreSome(t *testing.T) {
	root := t.TempDir()
	dir := v2Container(t, root, idA, 1, 1)
	collector := NewCollector(root, 0)

	points := collector.Collect([]Container{container("app", idA)}, time.Unix(1_700_000_000, 0))

	if _, ok := find(points, "container.cpu.limit_cores", nil); ok {
		t.Error("a limit that is not set was reported")
	}

	write(t, dir, "cpu.max", "200000 100000\n")
	write(t, dir, "memory.max", "1048576\n")

	points = collector.Collect([]Container{container("app", idA)}, time.Unix(1_700_000_030, 0))

	if point, ok := find(points, "container.cpu.limit_cores", nil); !ok || !near(point.Value, 2) {
		t.Errorf("cpu limit: %+v", point)
	}

	if point, ok := find(points, "container.memory.limit_bytes", nil); !ok || point.Value != 1048576 {
		t.Errorf("memory limit: %+v", point)
	}
}

func TestTheNumberOfPointsIsBoundedAndPodFiguresSurviveIt(t *testing.T) {
	root := t.TempDir()
	v2Container(t, root, idA, 1_000_000, 100)
	collector := NewCollector(root, 4)

	points := collector.Collect([]Container{container("app", idA)}, time.Unix(1_700_000_000, 0))

	if len(points) != 4 || collector.Dropped == 0 {
		t.Fatalf("got %d points, dropped %d", len(points), collector.Dropped)
	}

	if points[0].Name != "pod.memory.working_set_bytes" {
		t.Errorf("the pod figure is what is kept first, got %s", points[0].Name)
	}
}

func TestTheSameContainersGiveTheSameOrder(t *testing.T) {
	root := t.TempDir()
	v2Container(t, root, idA, 1, 1)
	v2Container(t, root, idB, 1, 1)

	a := NewCollector(root, 0).Collect([]Container{container("b", idB), container("a", idA)}, time.Unix(1, 0))
	b := NewCollector(root, 0).Collect([]Container{container("a", idA), container("b", idB)}, time.Unix(1, 0))

	if len(a) != len(b) {
		t.Fatal("different lengths")
	}

	for i := range a {
		if a[i].Name != b[i].Name || a[i].Labels["container"] != b[i].Labels["container"] {
			t.Fatalf("order differs at %d: %+v vs %+v", i, a[i], b[i])
		}
	}
}
