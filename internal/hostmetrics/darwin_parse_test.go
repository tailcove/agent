package hostmetrics

import "testing"

func TestParseDarwinCPUTakesTheLastSample(t *testing.T) {
	text := `Processes: 500 total
CPU usage: 40.00% user, 20.00% sys, 40.00% idle
SharedLibs: 1M resident
Processes: 501 total
CPU usage: 3.12% user, 5.47% sys, 91.40% idle
`

	got, ok := parseDarwinCPU(text)
	if !ok || got < 8.59 || got > 8.61 {
		t.Fatalf("got %v %v", got, ok)
	}

	for _, bad := range []string{"", "CPU usage: nonsense", "CPU usage: 1% user, 150% idle"} {
		if _, ok := parseDarwinCPU(bad); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestParseDarwinLoad(t *testing.T) {
	load, err := parseDarwinLoad("{ 1.52 1.60 1.65 }\n")
	if err != nil || load.One != 1.52 || load.Five != 1.60 || load.Fifteen != 1.65 {
		t.Fatalf("got %+v %v", load, err)
	}

	for _, bad := range []string{"", "{ 1 2 }", "{ a b c }"} {
		if _, err := parseDarwinLoad(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestParseVMStatAddsTheReclaimablePagesAtTheMachinesPageSize(t *testing.T) {
	text := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               10000.
Pages active:                            200000.
Pages inactive:                           50000.
Pages speculative:                         2000.
Pages wired down:                         90000.
Pages purgeable:                           8000.
`

	got, err := parseVMStat(text)
	if err != nil || got != (10000+50000+2000+8000)*16384 {
		t.Fatalf("got %d %v", got, err)
	}

	if _, err := parseVMStat("nothing useful"); err == nil {
		t.Error("output without free pages was accepted")
	}

	if got, _ := parseVMStat("Pages free: 10.\n"); got != 10*4096 {
		t.Errorf("default page size: %d", got)
	}
}

func TestParseDarwinSwap(t *testing.T) {
	total, free, ok := parseDarwinSwap("total = 2048.00M  used = 1056.75M  free = 991.25M  (encrypted)")
	if !ok || total != 2048<<20 || free != uint64(991.25*float64(1<<20)) {
		t.Fatalf("got %d %d %v", total, free, ok)
	}

	if _, _, ok := parseDarwinSwap("total = 0.00M  used = 0.00M"); ok {
		t.Error("swap without a free figure was accepted")
	}

	if got, ok := parseSize("1.5G"); !ok || got != uint64(1.5*float64(1<<30)) {
		t.Errorf("1.5G: %d %v", got, ok)
	}

	if got, ok := parseSize("17179869184"); !ok || got != 17179869184 {
		t.Errorf("plain bytes: %d %v", got, ok)
	}
}

func TestParseDarwinBoot(t *testing.T) {
	if sec, ok := parseDarwinBoot("{ sec = 1761000000, usec = 123456 } Mon Oct 20 22:40:00 2025"); !ok || sec != 1761000000 {
		t.Fatalf("got %d %v", sec, ok)
	}

	for _, bad := range []string{"", "{ usec = 1 }", "{ sec = x, usec = 1 }"} {
		if _, ok := parseDarwinBoot(bad); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestParseDarwinNetstatReadsTheCountersFromTheEndOfEachRow(t *testing.T) {
	text := `Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
lo0        16384 <Link#1>                           100     0       2000      100     0       2000     0
lo0        16384 127           127.0.0.1            100     -       2000      100     -       2000     -
en0        1500  <Link#6>      aa:bb:cc:dd:ee:ff  5000     2    7000000     4000     1    1000000     0
en0        1500  192.168.1     192.168.1.7        5000     -    7000000     4000     -    1000000     -
awdl0*     1500  <Link#9>      aa:bb:cc:dd:ee:00     0     0          0        0     0          0     0
`

	got := parseDarwinNetstat(text)
	if len(got) != 3 {
		t.Fatalf("got %d interfaces: %+v", len(got), got)
	}

	if got[0].Name != "lo0" || got[0].RxBytes != 2000 || got[0].RxPackets != 100 {
		t.Errorf("lo0 %+v", got[0])
	}

	if en0 := got[1]; en0.Name != "en0" || en0.RxPackets != 5000 || en0.RxErrors != 2 || en0.RxBytes != 7000000 || en0.TxPackets != 4000 || en0.TxErrors != 1 || en0.TxBytes != 1000000 {
		t.Errorf("en0 %+v", en0)
	}

	if got[2].Name != "awdl0" {
		t.Errorf("a starred name was kept: %q", got[2].Name)
	}
}

func TestParseDarwinMountsKeepsDisksAndDropsTheSystemsHelperVolumes(t *testing.T) {
	text := `/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
devfs on /dev (devfs, local, nobrowse)
/dev/disk3s6 on /System/Volumes/VM (apfs, local, noexec, journaled, noatime, nobrowse)
/dev/disk3s5 on /System/Volumes/Data (apfs, local, journaled, nobrowse, protect)
/dev/disk4s1 on /Volumes/External Disk (exfat, local, nodev, nosuid)
map auto_home on /System/Volumes/Data/home (autofs, automounted, nobrowse)
`

	mounts := parseDarwinMounts(text)

	var paths []string

	for _, mount := range mounts {
		paths = append(paths, mount.Path)
	}

	want := []string{"/", "/System/Volumes/Data", "/Volumes/External Disk"}
	if len(paths) != len(want) {
		t.Fatalf("paths %v", paths)
	}

	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("paths %v, want %v", paths, want)
		}
	}

	if mounts[0].Type != "apfs" || mounts[2].Type != "exfat" {
		t.Errorf("types %+v", mounts)
	}
}
