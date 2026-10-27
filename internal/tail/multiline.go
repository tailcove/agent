package tail

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Multiline modes.
const (
	MultilineOff     = "off"
	MultilineAuto    = "auto"
	MultilinePattern = "pattern"
)

// Defaults for the limits of one joined event.
const (
	DefaultMultilineLines   = 200
	DefaultMultilineBytes   = MaxLineBytes
	DefaultMultilineTimeout = time.Second
)

// Multiline joins the lines of one event (a stack trace) into a single entry.
//
// In "auto" mode a line continues the event before it when it is indented, or
// starts like the rest of a trace in common languages ("Caused by:", "... 12
// more", a PHP "#3 " frame, Python's Traceback header and the exception line
// that ends it). A line that starts with "{" always begins a new event, so a
// structured log is never glued to its neighbour. In "pattern" mode a line
// that matches Pattern begins a new event and every other line continues the
// one before it.
//
// An event is passed on when the next one begins, when its file has been quiet
// for Timeout, when it reaches MaxLines or MaxBytes (the rest starts a new
// event), or when its file goes away.
type Multiline struct {
	Mode     string
	Pattern  *regexp.Regexp
	MaxLines int
	MaxBytes int
	Timeout  time.Duration
}

var autoContinuation = regexp.MustCompile(`^(?:\s+\S|Caused by:|Suppressed:|\.\.\. \d+ more|#\d+\s|Stack trace:|\{main\}|Traceback \(most recent call last\):)`)

// exceptionLine is the line that ends a Python traceback: "ValueError: boom".
var exceptionLine = regexp.MustCompile(`^[A-Za-z_][\w.]*(?:Error|Exception|Warning|Exit|Interrupt)\b`)

// NewMultiline checks and fills in a multiline setting. An empty mode, or
// "off", means no joining and returns nil.
func NewMultiline(mode, pattern string, maxLines, maxBytes int, timeout time.Duration) (*Multiline, error) {
	switch mode {
	case "", MultilineOff:
		return nil, nil
	case MultilineAuto, MultilinePattern:
	default:
		return nil, fmt.Errorf("multiline mode %q is not off, auto or pattern", mode)
	}

	m := &Multiline{Mode: mode, MaxLines: maxLines, MaxBytes: maxBytes, Timeout: timeout}

	if mode == MultilinePattern {
		if pattern == "" {
			return nil, fmt.Errorf("multiline mode pattern needs a pattern: the regular expression that begins an event")
		}

		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("multiline pattern: %w", err)
		}

		m.Pattern = compiled
	}

	if m.MaxLines <= 0 {
		m.MaxLines = DefaultMultilineLines
	}

	if m.MaxBytes <= 0 || m.MaxBytes > MaxLineBytes {
		m.MaxBytes = DefaultMultilineBytes
	}

	if m.Timeout <= 0 {
		m.Timeout = DefaultMultilineTimeout
	}

	return m, nil
}

// event is what has been joined so far in one file.
type event struct {
	text      strings.Builder
	lines     int
	first     time.Time
	lastAt    time.Time // when the last line happened (its own time)
	last      time.Time // when the last line was added (the clock, for the idle timeout)
	stream    string
	offset    int64
	traceback bool // a Python traceback header was seen: the exception line is still to come
	framed    bool // the previous line was an indented frame of such a traceback
}

// continues says whether the line belongs to the event that is open.
func (m *Multiline) continues(line string, open *event) bool {
	if open == nil || open.lines == 0 {
		return false
	}

	if strings.HasPrefix(line, "{") {
		return false
	}

	if m.Mode == MultilinePattern {
		return !m.Pattern.MatchString(line)
	}

	if autoContinuation.MatchString(line) {
		// A traceback header inside an open event is a continuation; one that
		// starts the event is handled by the caller (nothing is open then).
		return true
	}

	// The line that names the exception ends a Python traceback.
	return open.traceback && open.framed && exceptionLine.MatchString(line)
}

// add puts a line into the open event, noting what kind of line it was.
func (e *event) add(text string, at time.Time, now time.Time, stream string, offset int64) {
	if e.lines == 0 {
		e.first = at
		e.stream = stream
	} else {
		e.text.WriteByte('\n')
	}

	e.text.WriteString(text)
	e.lines++
	e.last = now
	e.lastAt = at
	e.offset = offset

	switch {
	case strings.HasPrefix(text, "Traceback (most recent call last):"):
		e.traceback, e.framed = true, false
	case e.traceback && (strings.HasPrefix(text, " ") || strings.HasPrefix(text, "\t")):
		e.framed = true
	case e.traceback && exceptionLine.MatchString(text):
		// The exception line closes the traceback; a following indented line is not one of its frames.
		e.traceback, e.framed = false, false
	}
}
