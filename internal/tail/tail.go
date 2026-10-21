// Package tail follows log files: it finds the files a pattern matches, reads
// the lines appended to them, and survives rotation and truncation.
package tail

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MaxLineBytes is the longest line passed on whole; the rest of a longer one
// is dropped and the line is marked.
const MaxLineBytes = 64 * 1024

const truncationMark = " … [truncated]"

// Line is one line read from a file. Offset is where the file stood after it,
// so a consumer that has finished with the line can record how far it got.
type Line struct {
	File   string
	Inode  uint64
	Offset int64
	Text   string
	Time   time.Time
}

// Position is where reading of a file should resume.
type Position struct {
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

// Source is a path or glob, and where to start the first time a file is seen.
type Source struct {
	Pattern       string
	FromBeginning bool
}

// Follower reads every file its sources match and sends their lines on Lines.
type Follower struct {
	Sources []Source
	// Resume holds the saved position of each file by path.
	Resume map[string]Position
	// PollInterval is how often files are checked for new data.
	PollInterval time.Duration
	// RescanInterval is how often the patterns are expanded again.
	RescanInterval time.Duration
	Lines          chan<- Line

	mu      sync.Mutex
	files   map[string]*file
	started bool
}

type file struct {
	path    string
	handle  *os.File
	inode   uint64
	offset  int64
	pending []byte
	cut     bool
	idle    time.Time
}

// Run follows the files until ctx is done. It blocks while Lines is full, so a
// consumer that cannot keep up slows reading down instead of losing lines.
func (f *Follower) Run(ctx context.Context) {
	if f.PollInterval <= 0 {
		f.PollInterval = 250 * time.Millisecond
	}

	if f.RescanInterval <= 0 {
		f.RescanInterval = 5 * time.Second
	}

	f.files = map[string]*file{}
	f.rescan(true)
	f.started = true

	poll := time.NewTicker(f.PollInterval)
	rescan := time.NewTicker(f.RescanInterval)

	defer poll.Stop()
	defer rescan.Stop()
	defer f.closeAll()

	for {
		select {
		case <-ctx.Done():
			return
		case <-rescan.C:
			f.rescan(false)
		case <-poll.C:
			f.mu.Lock()
			paths := make([]string, 0, len(f.files))

			for path := range f.files {
				paths = append(paths, path)
			}

			f.mu.Unlock()
			sort.Strings(paths)

			for _, path := range paths {
				if ctx.Err() != nil {
					return
				}

				f.read(ctx, f.files[path])
			}
		}
	}
}

func (f *Follower) closeAll() {
	for _, tracked := range f.files {
		if tracked.handle != nil {
			_ = tracked.handle.Close()
		}
	}
}

// rescan opens files the patterns now match and have not been followed yet.
func (f *Follower) rescan(initial bool) {
	for _, source := range f.Sources {
		matches, err := filepath.Glob(source.Pattern)
		if err != nil {
			continue
		}

		for _, path := range matches {
			if _, tracked := f.files[path]; tracked {
				continue
			}

			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}

			f.open(path, info, source, initial)
		}
	}
}

func (f *Follower) open(path string, info os.FileInfo, source Source, initial bool) {
	handle, err := os.Open(path)
	if err != nil {
		return
	}

	inode := inodeOf(info)
	offset := int64(0)

	switch saved, known := f.Resume[path]; {
	case known && saved.Inode == inode && saved.Offset <= info.Size():
		offset = saved.Offset
	case known && saved.Inode == inode:
		// Truncated while the agent was away.
		offset = 0
	case initial && !source.FromBeginning:
		// First sight of a file that was already there: only what is
		// written from now on.
		offset = info.Size()
	default:
		// A file that appeared while running (or a rotated one the agent has
		// no position for) is read whole.
		offset = 0
	}

	if _, err := handle.Seek(offset, io.SeekStart); err != nil {
		_ = handle.Close()

		return
	}

	f.files[path] = &file{path: path, handle: handle, inode: inode, offset: offset, idle: time.Now()}
}

// read sends the complete lines now available in a file, and notices
// truncation and rotation.
func (f *Follower) read(ctx context.Context, tracked *file) {
	info, err := os.Stat(tracked.path)

	switch {
	case err != nil:
		// Gone (rotated away); drain what is left in the open handle, then
		// forget it. A new file at the path is picked up by the next rescan.
		f.drain(ctx, tracked)
		f.forget(tracked)

		return
	case inodeOf(info) != tracked.inode:
		// Replaced: finish the old file, then start the new one from its top.
		f.drain(ctx, tracked)
		f.forget(tracked)

		if handle, err := os.Open(tracked.path); err == nil {
			f.mu.Lock()
			f.files[tracked.path] = &file{path: tracked.path, handle: handle, inode: inodeOf(info), idle: time.Now()}
			f.mu.Unlock()
		}

		return
	case info.Size() < tracked.offset:
		// Truncated in place.
		tracked.offset = 0
		tracked.pending = nil
		tracked.cut = false

		if _, err := tracked.handle.Seek(0, io.SeekStart); err != nil {
			return
		}
	}

	f.drain(ctx, tracked)
}

func (f *Follower) forget(tracked *file) {
	_ = tracked.handle.Close()

	f.mu.Lock()
	delete(f.files, tracked.path)
	f.mu.Unlock()
}

func (f *Follower) drain(ctx context.Context, tracked *file) {
	reader := bufio.NewReaderSize(tracked.handle, 32*1024)

	for ctx.Err() == nil {
		chunk, err := reader.ReadSlice('\n')
		tracked.offset += int64(len(chunk))

		if room := MaxLineBytes - len(tracked.pending); room > 0 {
			tracked.pending = append(tracked.pending, chunk[:min(room, len(chunk))]...)
			tracked.cut = tracked.cut || len(chunk) > room
		} else {
			tracked.cut = true
		}

		switch {
		case err == nil:
			f.emit(ctx, tracked)
			tracked.idle = time.Now()
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			// EOF (or an error): a partial line stays pending until its
			// newline arrives. One that sits for 5 seconds is sent as it is.
			if len(tracked.pending) > 0 && time.Since(tracked.idle) > 5*time.Second {
				f.emit(ctx, tracked)
			}

			return
		}
	}
}

func (f *Follower) emit(ctx context.Context, tracked *file) {
	text := strings.TrimRight(string(tracked.pending), "\r\n")

	if tracked.cut {
		text += truncationMark
	}

	tracked.pending = nil
	tracked.cut = false

	if text == "" {
		return
	}

	line := Line{File: tracked.path, Inode: tracked.inode, Offset: tracked.offset, Text: text, Time: time.Now()}

	select {
	case f.Lines <- line:
	case <-ctx.Done():
	}
}

func inodeOf(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Ino)
	}

	return 0
}
