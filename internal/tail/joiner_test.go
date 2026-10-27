package tail

import (
	"testing"
	"time"
)

func TestJoinerPassesLinesThroughWithoutASetting(t *testing.T) {
	joiner := NewJoiner(nil)
	at := time.Now()

	got := joiner.Add("a", at, at, "stdout")
	if len(got) != 1 || got[0].Text != "a" || got[0].Lines != 1 {
		t.Fatalf("got %+v", got)
	}

	if _, ok := joiner.Flush(); ok {
		t.Fatal("nothing should be open")
	}
}

func TestJoinerJoinsATraceAndEndsItAtTheNextEvent(t *testing.T) {
	multiline, err := NewMultiline(MultilineAuto, "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	joiner := NewJoiner(multiline)
	at := time.Unix(100, 0)

	var out []Joined

	for _, line := range []string{"Exception: boom", "    at one()", "    at two()", "next event"} {
		out = append(out, joiner.Add(line, at, at, "stderr")...)
	}

	if len(out) != 1 || out[0].Text != "Exception: boom\n    at one()\n    at two()" || out[0].Lines != 3 {
		t.Fatalf("got %+v", out)
	}

	rest, ok := joiner.Flush()
	if !ok || rest.Text != "next event" {
		t.Fatalf("rest %+v %v", rest, ok)
	}
}

func TestJoinerKeepsStreamsApart(t *testing.T) {
	multiline, _ := NewMultiline(MultilineAuto, "", 0, 0, 0)
	joiner := NewJoiner(multiline)
	at := time.Unix(100, 0)

	joiner.Add("error line", at, at, "stderr")

	done := joiner.Add("    indented but on stdout", at, at, "stdout")
	if len(done) != 1 || done[0].Stream != "stderr" {
		t.Fatalf("got %+v", done)
	}
}

func TestJoinerCutsAnEventAtTheLineAndByteLimits(t *testing.T) {
	multiline, _ := NewMultiline(MultilineAuto, "", 3, 0, 0)
	joiner := NewJoiner(multiline)
	at := time.Unix(100, 0)

	var out []Joined

	for _, line := range []string{"head", "  a", "  b", "  c"} {
		out = append(out, joiner.Add(line, at, at, "stdout")...)
	}

	if len(out) != 1 || out[0].Lines != 3 {
		t.Fatalf("got %+v", out)
	}
}

func TestJoinerSendsAnIdleEventAfterTheTimeout(t *testing.T) {
	multiline, _ := NewMultiline(MultilineAuto, "", 0, 0, time.Second)
	joiner := NewJoiner(multiline)
	start := time.Unix(100, 0)

	joiner.Add("Exception: x", start, start, "stderr")

	if _, ok := joiner.Idle(start.Add(500 * time.Millisecond)); ok {
		t.Fatal("too early")
	}

	got, ok := joiner.Idle(start.Add(time.Second))
	if !ok || got.Text != "Exception: x" {
		t.Fatalf("got %+v %v", got, ok)
	}
}
