package tail

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustMultiline(t *testing.T, mode, pattern string) *Multiline {
	t.Helper()

	m, err := NewMultiline(mode, pattern, 0, 0, 80*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	return m
}

func joined(t *testing.T, m *Multiline, input string, want int) []Line {
	t.Helper()

	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, input)

	lines, _ := start(t, &Follower{Sources: []Source{{Pattern: path, FromBeginning: true, Multiline: m}}})

	var got []Line

	for range want {
		got = append(got, next(t, lines))
	}

	return got
}

func TestAJavaStackTraceIsOneEntry(t *testing.T) {
	got := joined(t, mustMultiline(t, MultilineAuto, ""), strings.Join([]string{
		"2026-10-27 10:00:00 ERROR Something failed",
		"java.lang.IllegalStateException: boom",
		"\tat com.acme.Cart.add(Cart.java:42)",
		"\tat com.acme.Shop.run(Shop.java:7)",
		"Caused by: java.io.IOException: disk",
		"\t... 12 more",
		"2026-10-27 10:00:01 INFO Next thing",
		"",
	}, "\n"), 3)

	// "java.lang..." is not indented, so it starts an entry of its own; the trace that follows is joined to it.
	if got[0].Text != "2026-10-27 10:00:00 ERROR Something failed" {
		t.Errorf("first: %q", got[0].Text)
	}

	if !strings.HasPrefix(got[1].Text, "java.lang.IllegalStateException: boom\n\tat com.acme.Cart.add") || !strings.Contains(got[1].Text, "Caused by: java.io.IOException: disk\n\t... 12 more") {
		t.Errorf("second: %q", got[1].Text)
	}

	if got[2].Text != "2026-10-27 10:00:01 INFO Next thing" {
		t.Errorf("third: %q", got[2].Text)
	}
}

func TestAPythonTracebackIncludingItsLastLineIsOneEntry(t *testing.T) {
	got := joined(t, mustMultiline(t, MultilineAuto, ""), strings.Join([]string{
		"ERROR request failed",
		"Traceback (most recent call last):",
		`  File "app.py", line 3, in handler`,
		"    1 / 0",
		"ZeroDivisionError: division by zero",
		"INFO next request",
		"",
	}, "\n"), 2)

	want := "ERROR request failed\nTraceback (most recent call last):\n  File \"app.py\", line 3, in handler\n    1 / 0\nZeroDivisionError: division by zero"

	if got[0].Text != want || got[1].Text != "INFO next request" {
		t.Errorf("got %q and %q", got[0].Text, got[1].Text)
	}
}

func TestAPHPStackTraceIsOneEntry(t *testing.T) {
	got := joined(t, mustMultiline(t, MultilineAuto, ""), strings.Join([]string{
		"[2026-10-27 10:00:00] production.ERROR: Boom {\"exception\":\"x\"}",
		"[stacktrace]",
		"#0 /app/Cart.php(42): Cart->add()",
		"#1 {main}",
		"[2026-10-27 10:00:01] production.INFO: ok",
		"",
	}, "\n"), 3)

	// "[stacktrace]" starts with "[", so it is its own entry in auto mode; the frames join it.
	if !strings.HasPrefix(got[1].Text, "[stacktrace]\n#0 /app/Cart.php(42)") || !strings.HasSuffix(got[1].Text, "#1 {main}") {
		t.Errorf("got %q", got[1].Text)
	}
}

func TestAStructuredLineIsNeverGluedToTheOneBeforeIt(t *testing.T) {
	got := joined(t, mustMultiline(t, MultilineAuto, ""), "plain start\n  indented but belongs\n{\"message\":\"json\"}\n{\"message\":\"json 2\"}\n", 3)

	if got[0].Text != "plain start\n  indented but belongs" || got[1].Text != `{"message":"json"}` || got[2].Text != `{"message":"json 2"}` {
		t.Errorf("got %q %q %q", got[0].Text, got[1].Text, got[2].Text)
	}
}

func TestPatternModeStartsAnEntryAtEveryMatch(t *testing.T) {
	got := joined(t, mustMultiline(t, MultilinePattern, `^\d{4}-\d{2}-\d{2} `), strings.Join([]string{
		"2026-10-27 10:00:00 first",
		"no indentation but still the first",
		"2026-10-27 10:00:01 second",
		"",
	}, "\n"), 2)

	if got[0].Text != "2026-10-27 10:00:00 first\nno indentation but still the first" || got[1].Text != "2026-10-27 10:00:01 second" {
		t.Errorf("got %q %q", got[0].Text, got[1].Text)
	}
}

func TestTheOffsetOfAJoinedEntryIsThatOfItsLastLine(t *testing.T) {
	input := "head\n  one\n  two\n"
	got := joined(t, mustMultiline(t, MultilineAuto, ""), input+"head 2\n", 2)

	if got[0].Offset != int64(len(input)) {
		t.Errorf("offset %d, want %d: a restart must resume after the whole entry, never inside it", got[0].Offset, len(input))
	}
}

func TestAnEntryWaitingForItsNextLineIsSentWhenTheFileGoesQuiet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "lonely start\n  and a frame\n")

	lines, _ := start(t, &Follower{Sources: []Source{{Pattern: path, FromBeginning: true, Multiline: mustMultiline(t, MultilineAuto, "")}}})

	got := next(t, lines)

	if got.Text != "lonely start\n  and a frame" {
		t.Errorf("got %q", got.Text)
	}
}

func TestAnEntryIsSplitAtItsLimits(t *testing.T) {
	m, err := NewMultiline(MultilineAuto, "", 3, 0, 80*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	got := joined(t, m, "head\n  1\n  2\n  3\n  4\n", 2)

	if got[0].Text != "head\n  1\n  2" || got[1].Text != "  3\n  4" {
		t.Errorf("got %q and %q", got[0].Text, got[1].Text)
	}

	big, err := NewMultiline(MultilineAuto, "", 0, 1024, 80*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	long := strings.Repeat("x", 600)
	parts := joined(t, big, "head\n  "+long+"\n  "+long+"\n", 2)

	if len(parts[0].Text) > 1024 {
		t.Errorf("an entry grew past its byte limit: %d", len(parts[0].Text))
	}
}

func TestMultilineSettingsAreChecked(t *testing.T) {
	for name, mode := range map[string][2]string{
		"unknown mode":    {"smart", ""},
		"pattern missing": {MultilinePattern, ""},
		"bad pattern":     {MultilinePattern, "("},
	} {
		if _, err := NewMultiline(mode[0], mode[1], 0, 0, 0); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	for _, mode := range []string{"", MultilineOff} {
		if m, err := NewMultiline(mode, "", 0, 0, 0); m != nil || err != nil {
			t.Errorf("mode %q should mean no joining", mode)
		}
	}

	m, _ := NewMultiline(MultilineAuto, "", 0, 10_000_000, 0)

	if m.MaxBytes != MaxLineBytes || m.MaxLines != DefaultMultilineLines || m.Timeout != DefaultMultilineTimeout {
		t.Errorf("defaults not applied: %+v", m)
	}
}

func TestMultilineAndContainerFormatWorkTogether(t *testing.T) {
	root := t.TempDir()
	path := podFile(t, root, "0.log")
	appendTo(t, path, cri("stderr", "F", "panic: boom")+cri("stderr", "F", "goroutine 1 [running]:")+cri("stderr", "F", "\tmain.main()")+cri("stdout", "F", "back to normal"))

	source := criSource(root)
	source.Multiline = mustMultiline(t, MultilineAuto, "")
	source.Multiline.Pattern = nil
	lines, _ := start(t, &Follower{Sources: []Source{source}})

	first := next(t, lines)

	// "goroutine 1 [running]:" is not indented, so it begins its own entry; the indented frame joins it.
	if first.Text != "panic: boom" || first.Stream != "stderr" {
		t.Fatalf("got %+v", first)
	}

	second := next(t, lines)

	if second.Text != "goroutine 1 [running]:\n\tmain.main()" {
		t.Fatalf("got %q", second.Text)
	}
}
