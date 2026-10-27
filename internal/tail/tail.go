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
	// Stream is stdout or stderr for a container log, empty for any other file.
	Stream string
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
	// Accept, when set, is asked about every file the pattern matches; a file
	// it refuses is not followed.
	Accept func(path string) bool
	// Format is how the file's lines are written: raw (the default), cri,
	// docker or auto. See decode.
	Format string
	// Multiline, when set, joins the lines of one event into one line.
	Multiline *Multiline
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
	// PartialAfter is how long a line without its newline (or the pieces of a
	// long container line without their last piece) waits before it is sent as
	// it is; five seconds by default.
	PartialAfter time.Duration
	// Gone, when set, is called with the path of a file that was removed (a
	// pod that was deleted), after its last lines have been sent.
	Gone func(path string)

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

	source Source
	// piece collects the P pieces of a container line until its F piece.
	piece     strings.Builder
	piecing   bool
	pieceAt   time.Time
	pieceDeco decoded
	// open is the multiline event being joined.
	open *event
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

				if tracked := f.tracked(path); tracked != nil {
					f.read(ctx, tracked)
					f.flushIdle(ctx, tracked)
				}
			}
		}
	}
}

func (f *Follower) partialAfter() time.Duration {
	if f.PartialAfter > 0 {
		return f.PartialAfter
	}

	return 5 * time.Second
}

// tracked is the file followed at path, or nil when it has been forgotten
// since the list was taken.
func (f *Follower) tracked(path string) *file {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.files[path]
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

			if source.Accept != nil && !source.Accept(path) {
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

	f.mu.Lock()
	f.files[path] = &file{path: path, handle: handle, inode: inode, offset: offset, idle: time.Now(), source: source}
	f.mu.Unlock()
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
		f.finish(ctx, tracked)
		f.forget(tracked)

		if f.Gone != nil && ctx.Err() == nil {
			f.Gone(tracked.path)
		}

		return
	case inodeOf(info) != tracked.inode:
		// Replaced: finish the old file, then start the new one from its top.
		f.drain(ctx, tracked)
		f.finish(ctx, tracked)
		f.forget(tracked)

		if handle, err := os.Open(tracked.path); err == nil {
			f.mu.Lock()
			f.files[tracked.path] = &file{path: tracked.path, handle: handle, inode: inodeOf(info), idle: time.Now(), source: tracked.source}
			f.mu.Unlock()
		}

		return
	case info.Size() < tracked.offset:
		// Truncated in place.
		tracked.offset = 0
		tracked.pending = nil
		tracked.cut = false
		tracked.piece.Reset()
		tracked.piecing = false
		tracked.open = nil

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
			if len(tracked.pending) > 0 && time.Since(tracked.idle) > f.partialAfter() {
				f.emit(ctx, tracked)
			}

			return
		}
	}
}

// emit takes one physical line off the file's buffer and runs it through the
// source's format, piece joining and multiline joining.
func (f *Follower) emit(ctx context.Context, tracked *file) {
	text := strings.TrimRight(string(tracked.pending), "\r\n")

	if tracked.cut {
		text += truncationMark
	}

	tracked.pending = nil
	tracked.cut = false

	if text == "" && tracked.source.Format != FormatCRI && tracked.source.Format != FormatDocker && tracked.source.Format != FormatAuto {
		return
	}

	f.process(ctx, tracked, text)
}

// process decodes a line, joins the pieces of a long one, and hands the whole
// line to the multiline joiner (or sends it).
func (f *Follower) process(ctx context.Context, tracked *file, text string) {
	now := time.Now()
	d := decode(tracked.source.Format, text, now)

	if d.partial {
		if !tracked.piecing {
			tracked.piecing, tracked.pieceAt, tracked.pieceDeco = true, d.time, d
		}

		if room := MaxLineBytes - tracked.piece.Len(); room > 0 {
			tracked.piece.WriteString(d.text[:min(room, len(d.text))])
		}

		return
	}

	if tracked.piecing {
		whole := tracked.piece.String()
		tracked.piece.Reset()
		tracked.piecing = false

		if room := MaxLineBytes - len(whole); room > 0 {
			whole += d.text[:min(room, len(d.text))]
		}

		d.text, d.time = whole, tracked.pieceAt
	}

	if d.text == "" {
		return
	}

	f.join(ctx, tracked, d, now)
}

// join adds a whole line to the open multiline event, passing on the event
// before it when this line begins a new one.
func (f *Follower) join(ctx context.Context, tracked *file, d decoded, now time.Time) {
	multiline := tracked.source.Multiline

	if multiline == nil {
		f.send(ctx, tracked, d.text, d.time, d.stream, tracked.offset)

		return
	}

	if !multiline.continues(d.text, tracked.open) {
		f.flushEvent(ctx, tracked)
		tracked.open = &event{}
	}

	open := tracked.open

	if open.lines >= multiline.MaxLines || open.text.Len()+1+len(d.text) > multiline.MaxBytes {
		// Too much for one entry: pass it on and carry on in a new one.
		f.flushEvent(ctx, tracked)

		tracked.open = &event{}
		open = tracked.open
	}

	open.add(d.text, d.time, now, d.stream, tracked.offset)
}

// flushEvent sends the open multiline event, if there is one.
func (f *Follower) flushEvent(ctx context.Context, tracked *file) {
	open := tracked.open
	tracked.open = nil

	if open == nil || open.lines == 0 {
		return
	}

	f.send(ctx, tracked, open.text.String(), open.first, open.stream, open.offset)
}

// flushIdle passes on what has been waiting too long: a multiline event whose
// file has gone quiet, and the pieces of a long container line that never got
// their last piece.
func (f *Follower) flushIdle(ctx context.Context, tracked *file) {
	if tracked.open != nil && tracked.source.Multiline != nil && time.Since(tracked.open.last) >= tracked.source.Multiline.Timeout {
		f.flushEvent(ctx, tracked)
	}

	if tracked.piecing && time.Since(tracked.idle) > f.partialAfter() {
		f.finishPieces(ctx, tracked)
	}
}

// finishPieces sends the pieces collected so far as a line of their own.
func (f *Follower) finishPieces(ctx context.Context, tracked *file) {
	whole, d := tracked.piece.String(), tracked.pieceDeco

	tracked.piece.Reset()
	tracked.piecing = false

	if whole != "" {
		f.join(ctx, tracked, decoded{text: whole, time: tracked.pieceAt, stream: d.stream}, time.Now())
	}
}

// finish passes on everything still held for a file that is going away.
func (f *Follower) finish(ctx context.Context, tracked *file) {
	if tracked.piecing {
		f.finishPieces(ctx, tracked)
	}

	f.flushEvent(ctx, tracked)
}

// send puts a line on the channel.
func (f *Follower) send(ctx context.Context, tracked *file, text string, at time.Time, stream string, offset int64) {
	if at.IsZero() {
		at = time.Now()
	}

	line := Line{File: tracked.path, Inode: tracked.inode, Offset: offset, Text: text, Time: at, Stream: stream}

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
