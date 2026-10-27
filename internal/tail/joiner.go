package tail

import "time"

// Joined is one event made of one or more lines.
type Joined struct {
	Text string
	Time time.Time
	// Last is when the last of its lines happened.
	Last   time.Time
	Stream string
	Lines  int
}

// Joiner joins the lines of one stream (a container's stdout, say) into events
// with the same rules the file follower uses. It holds no clock and starts no
// goroutine: the caller says what time it is, and calls Idle now and then so an
// event is not held back for ever.
type Joiner struct {
	multiline *Multiline
	open      event
}

// NewJoiner returns a joiner for the setting; a nil setting passes every line
// through as its own event.
func NewJoiner(multiline *Multiline) *Joiner {
	return &Joiner{multiline: multiline}
}

// Add takes a line that happened at `at` and was seen at `now`. It returns the
// events that this line completed: none while a trace is still being collected.
func (j *Joiner) Add(text string, at, now time.Time, stream string) []Joined {
	if j.multiline == nil {
		return []Joined{{Text: text, Time: at, Last: at, Stream: stream, Lines: 1}}
	}

	var done []Joined

	switch {
	case j.open.lines == 0:
	case j.open.stream != stream:
		done = append(done, j.flush())
	case !j.multiline.continues(text, &j.open),
		j.open.lines >= j.multiline.MaxLines,
		j.open.text.Len()+1+len(text) > j.multiline.MaxBytes:
		done = append(done, j.flush())
	}

	j.open.add(text, at, now, stream, 0)

	return done
}

// Idle returns the open event when nothing has been added to it for the
// setting's timeout.
func (j *Joiner) Idle(now time.Time) (Joined, bool) {
	if j.multiline == nil || j.open.lines == 0 || now.Sub(j.open.last) < j.multiline.Timeout {
		return Joined{}, false
	}

	return j.flush(), true
}

// OpenSince is when the first line of the open event happened, if one is open.
func (j *Joiner) OpenSince() (time.Time, bool) {
	if j.multiline == nil || j.open.lines == 0 {
		return time.Time{}, false
	}

	return j.open.first, true
}

// Flush returns whatever is open, for when the stream ends.
func (j *Joiner) Flush() (Joined, bool) {
	if j.open.lines == 0 {
		return Joined{}, false
	}

	return j.flush(), true
}

func (j *Joiner) flush() Joined {
	joined := Joined{Text: j.open.text.String(), Time: j.open.first, Last: j.open.lastAt, Stream: j.open.stream, Lines: j.open.lines}
	j.open = event{}

	return joined
}
