package containermetrics

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxWalkDepth bounds the search for a container's cgroup below a root:
// kubepods / qos class / pod / container is four levels, the systemd driver adds
// a few slices.
const maxWalkDepth = 6

var containerHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Version of the cgroup hierarchy on the node.
type Version int

// The two cgroup versions.
const (
	V1 Version = 1
	V2 Version = 2
)

// DetectVersion says whether root is a cgroup v2 mount (it has cgroup.controllers).
func DetectVersion(root string) Version {
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err == nil {
		return V2
	}

	return V1
}

// Dirs are the directories of one container's cgroup: one in v2, a cpu and a
// memory one in v1.
type Dirs struct {
	CPU    string
	Memory string
}

// Locator finds container cgroups by container id and remembers where.
type Locator struct {
	Root    string
	Version Version

	found map[string]Dirs
}

// NewLocator detects the cgroup version under root.
func NewLocator(root string) *Locator {
	return &Locator{Root: root, Version: DetectVersion(root), found: map[string]Dirs{}}
}

// Find returns the cgroup directories of the container, or false when it has
// none (it is not running, or is not on this node).
func (l *Locator) Find(id string) (Dirs, bool) {
	if !containerHex.MatchString(id) {
		return Dirs{}, false
	}

	if dirs, known := l.found[id]; known && l.exists(dirs) {
		return dirs, true
	}

	delete(l.found, id)

	var dirs Dirs

	if l.Version == V2 {
		dirs.CPU = l.search([]string{"kubepods.slice", "kubepods"}, id)
		dirs.Memory = dirs.CPU
	} else {
		dirs.CPU = l.search([]string{"cpu,cpuacct/kubepods.slice", "cpu,cpuacct/kubepods", "cpu/kubepods"}, id)
		dirs.Memory = l.search([]string{"memory/kubepods.slice", "memory/kubepods"}, id)
	}

	if dirs.CPU == "" || dirs.Memory == "" {
		return Dirs{}, false
	}

	l.found[id] = dirs

	return dirs, true
}

func (l *Locator) exists(dirs Dirs) bool {
	for _, dir := range []string{dirs.CPU, dirs.Memory} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return false
		}
	}

	return true
}

// search walks the trees under the roots for a directory named after the id
// (cri-containerd-<id>.scope, crio-<id>.scope, docker-<id>.scope, or the bare id
// with the cgroupfs driver). Nothing outside a root is read.
func (l *Locator) search(roots []string, id string) string {
	for _, root := range roots {
		base := filepath.Join(l.Root, filepath.FromSlash(root))

		var match string

		_ = filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return fs.SkipDir
			}

			if !entry.IsDir() {
				return nil
			}

			if depth := strings.Count(strings.TrimPrefix(path, base), string(filepath.Separator)); depth > maxWalkDepth {
				return fs.SkipDir
			}

			if name := entry.Name(); name == id || (strings.HasSuffix(name, ".scope") && strings.Contains(name, id)) {
				match = path

				return fs.SkipAll
			}

			return nil
		})

		if match != "" {
			return match
		}
	}

	return ""
}

// Read takes one reading of a container.
func (l *Locator) Read(dirs Dirs) Stats {
	read := func(dir, name string) string {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ""
		}

		return string(raw)
	}

	if l.Version == V2 {
		return ParseV2(
			read(dirs.CPU, "cpu.stat"), read(dirs.CPU, "cpu.max"),
			read(dirs.Memory, "memory.current"), read(dirs.Memory, "memory.stat"),
			read(dirs.Memory, "memory.max"), read(dirs.Memory, "memory.events"),
		)
	}

	return ParseV1(
		read(dirs.CPU, "cpuacct.usage"), read(dirs.CPU, "cpu.stat"),
		read(dirs.CPU, "cpu.cfs_quota_us"), read(dirs.CPU, "cpu.cfs_period_us"),
		read(dirs.Memory, "memory.usage_in_bytes"), read(dirs.Memory, "memory.stat"),
		read(dirs.Memory, "memory.limit_in_bytes"), read(dirs.Memory, "memory.oom_control"),
	)
}
