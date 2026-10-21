package agent

import (
	"os"
	"syscall"
)

func inodeOfForTest(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Ino)
	}

	return 0
}
