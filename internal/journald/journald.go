// Package journald follows systemd's journal by running journalctl, which is
// read-only and ships with systemd, so the agent needs no library and no cgo.
package journald

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tailcove/agent/internal/source"
)

// Key is the name of the journal's cursor in the state file.
const Key = "journald"

// maxLine bounds one JSON entry read from journalctl. A journal field can be
// large (a core dump's message), so a longer entry is skipped, not buffered.
const maxLine = 1 << 20

// maxMessage bounds the message that is sent.
const maxMessage = 64 << 10

// Levels by syslog priority, 0 (emergency) to 7 (debug); the log endpoint takes all of them.
var levels = [8]string{"emergency", "alert", "critical", "error", "warning", "notice", "info", "debug"}

// Priorities are the names a minimum priority may be given by.
func Priority(name string) (int, bool) {
	name = strings.ToLower(strings.TrimSpace(name))

	switch name {
	case "":
		return 7, true
	case "warn":
		return 4, true
	case "err":
		return 3, true
	case "crit":
		return 2, true
	case "emerg":
		return 0, true
	}

	for priority, level := range levels {
		if name == level {
			return priority, true
		}
	}

	return 0, false
}

// Reader follows the journal and emits one event per entry.
type Reader struct {
	// Path is the journalctl program ("journalctl").
	Path string
	// Directory reads the journal files of another directory (a copied journal, a mounted host's /var/log/journal).
	Directory string
	// Units limit the entries to these units (a glob such as "nginx*" is allowed by journalctl).
	Units []string
	// MaxPriority is the highest priority number sent: 7 sends everything, 4 only warnings and worse.
	MaxPriority int
	// FromBeginning starts at the oldest entry when there is no cursor; otherwise a first run starts at now.
	FromBeginning bool
	// Cursor is where delivery stopped the last time ("" when unknown).
	Cursor string
	// Host is named by every entry that has no hostname of its own.
	Host string

	Emit func(source.Event)
	Logf func(format string, args ...any)

	// Command builds the process; tests replace it.
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd

	// RetryAfter is the wait before journalctl is started again after it stopped.
	RetryAfter time.Duration
}

// args is the journalctl command line. Every argument is built here from
// configuration that was validated, and no shell is involved.
func (r *Reader) args(cursor string) []string {
	args := []string{"--output=json", "--no-pager", "--follow", "--quiet"}

	switch {
	case cursor != "":
		args = append(args, "--after-cursor="+cursor)
	case r.FromBeginning:
		args = append(args, "--lines=all")
	default:
		args = append(args, "--lines=0")
	}

	if r.Directory != "" {
		args = append(args, "--directory="+r.Directory)
	}

	if r.MaxPriority < 7 {
		args = append(args, "--priority="+strconv.Itoa(r.MaxPriority))
	}

	for _, unit := range r.Units {
		args = append(args, "--unit="+unit)
	}

	return args
}

// Run follows the journal until the context ends. journalctl stops when the
// journal is rotated away or the machine is busy enough to kill it; it is
// started again from the last entry that was read.
func (r *Reader) Run(ctx context.Context) {
	if r.Path == "" {
		r.Path = "journalctl"
	}

	if r.Command == nil {
		r.Command = exec.CommandContext
	}

	if r.RetryAfter <= 0 {
		r.RetryAfter = 5 * time.Second
	}

	cursor := r.Cursor
	failures := 0

	for ctx.Err() == nil {
		started := time.Now()
		last, err := r.once(ctx, cursor)

		if last != "" {
			cursor = last
		}

		if ctx.Err() != nil {
			return
		}

		// A run that lasted is not a failure to count; one that dies at once is
		// said once, then retried quietly.
		if time.Since(started) > time.Minute {
			failures = 0
		}

		if failures == 0 {
			r.logf("journald: journalctl stopped (%v); starting it again", err)
		}

		failures++

		select {
		case <-ctx.Done():
			return
		case <-time.After(r.RetryAfter):
		}
	}
}

// once runs journalctl until it ends and returns the cursor of the last entry read.
func (r *Reader) once(ctx context.Context, cursor string) (string, error) {
	cmd := r.Command(ctx, r.Path, r.args(cursor)...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}

	var errors bytes.Buffer

	cmd.Stderr = &limitedWriter{buffer: &errors, limit: 2048}

	if err := cmd.Start(); err != nil {
		return "", err
	}

	last := r.read(stdout)
	err = cmd.Wait()

	if err != nil && errors.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(errors.String()))
	}

	return last, err
}

// read turns journalctl's output into events until it ends.
func (r *Reader) read(stream io.Reader) string {
	reader := bufio.NewReaderSize(stream, 64<<10)
	last := ""

	for {
		line, tooLong, err := readLine(reader)

		if len(line) > 0 && !tooLong {
			if event, ok := r.entry(line); ok {
				if r.Emit != nil {
					r.Emit(event)
				}

				last = event.Cursor
			}
		}

		if err != nil {
			return last
		}
	}
}

// readLine reads one line of at most maxLine bytes; a longer one is read to its
// end and reported as too long, so it is skipped without being held in memory.
func readLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte

	for {
		part, isPrefix, err := reader.ReadLine()
		if err != nil {
			return line, false, err
		}

		if len(line)+len(part) > maxLine {
			for isPrefix {
				if _, isPrefix, err = reader.ReadLine(); err != nil {
					return nil, true, err
				}
			}

			return nil, true, nil
		}

		line = append(line, part...)

		if !isPrefix {
			return line, false, nil
		}
	}
}

// entry turns one JSON line of journalctl into the object the log endpoint takes.
func (r *Reader) entry(line []byte) (source.Event, bool) {
	var fields map[string]json.RawMessage

	if err := json.Unmarshal(line, &fields); err != nil {
		return source.Event{}, false
	}

	cursor := text(fields["__CURSOR"])
	if cursor == "" {
		return source.Event{}, false
	}

	message := text(fields["MESSAGE"])
	if message == "" {
		// A binary message arrives as an array of byte values.
		message = binaryMessage(fields["MESSAGE"])
	}

	if len(message) > maxMessage {
		message = message[:maxMessage]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}

	entry := map[string]any{"message": message}

	if priority, err := strconv.Atoi(text(fields["PRIORITY"])); err == nil && priority >= 0 && priority <= 7 {
		entry["level"] = levels[priority]
	}

	if micros, err := strconv.ParseInt(text(fields["__REALTIME_TIMESTAMP"]), 10, 64); err == nil && micros > 0 {
		entry["ts"] = time.UnixMicro(micros).UTC().Format(time.RFC3339Nano)
	}

	host := text(fields["_HOSTNAME"])
	if host == "" {
		host = r.Host
	}

	if host != "" {
		entry["host"] = host
	}

	context := map[string]string{}

	for field, key := range map[string]string{
		"_SYSTEMD_UNIT":      "unit",
		"_SYSTEMD_USER_UNIT": "user_unit",
		"SYSLOG_IDENTIFIER":  "identifier",
		"_COMM":              "command",
		"_PID":               "pid",
		"_TRANSPORT":         "transport",
		"CONTAINER_NAME":     "container",
	} {
		if value := text(fields[field]); value != "" {
			context[key] = value
		}
	}

	if len(context) > 0 {
		entry["context"] = map[string]any{"journald": context}
	}

	encoded, err := json.Marshal(entry)
	if err != nil {
		return source.Event{}, false
	}

	return source.Event{Entry: encoded, Key: Key, Cursor: cursor}, true
}

// text is a journal field as a string. The journal writes a field that has
// several values as an array; the first is used.
func text(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single
	}

	var many []json.RawMessage
	if json.Unmarshal(raw, &many) == nil && len(many) > 0 {
		var first string
		if json.Unmarshal(many[0], &first) == nil {
			return first
		}
	}

	return ""
}

// binaryMessage describes a message that was not text.
func binaryMessage(raw json.RawMessage) string {
	var values []int
	if json.Unmarshal(raw, &values) == nil && len(values) > 0 {
		return fmt.Sprintf("[binary message of %d bytes]", len(values))
	}

	return ""
}

func (r *Reader) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// limitedWriter keeps the first bytes written to it, for an error message.
type limitedWriter struct {
	mu     sync.Mutex
	buffer *bytes.Buffer
	limit  int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if room := w.limit - w.buffer.Len(); room > 0 {
		w.buffer.Write(p[:min(room, len(p))])
	}

	return len(p), nil
}
