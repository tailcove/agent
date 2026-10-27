package journald

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tailcove/agent/internal/source"
)

// TestHelperProcess is journalctl for the tests below: it prints what the
// JOURNALD_HELPER_OUTPUT variable holds, and records how it was started.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("JOURNALD_HELPER") != "1" {
		return
	}

	if record := os.Getenv("JOURNALD_HELPER_ARGS"); record != "" {
		handle, _ := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		_, _ = handle.WriteString(strings.Join(os.Args[3:], " ") + "\n")
		_ = handle.Close()
	}

	// The first start prints the entries; a later one (after a cursor) prints the second set.
	output := os.Getenv("JOURNALD_HELPER_OUTPUT")
	if strings.Contains(strings.Join(os.Args, " "), "--after-cursor=") {
		output = os.Getenv("JOURNALD_HELPER_OUTPUT_AFTER")
	}

	fmt.Print(output)

	if code := os.Getenv("JOURNALD_HELPER_EXIT"); code != "" {
		fmt.Fprint(os.Stderr, "journal went away")
		os.Exit(1)
	}

	// Stay alive like journalctl --follow until the test cancels.
	time.Sleep(time.Minute)
}

func helper(env ...string) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperProcess", "--"}, args...)...)
		cmd.Env = append(os.Environ(), append([]string{"JOURNALD_HELPER=1"}, env...)...)

		return cmd
	}
}

func entryJSON(cursor string, fields map[string]any) string {
	fields["__CURSOR"] = cursor
	encoded, _ := json.Marshal(fields)

	return string(encoded) + "\n"
}

func TestArgumentsFollowTheSettings(t *testing.T) {
	cases := []struct {
		name   string
		reader Reader
		cursor string
		want   []string
		not    []string
	}{
		{"a first run starts at now", Reader{MaxPriority: 7}, "", []string{"--lines=0", "--follow", "--output=json"}, []string{"--priority", "--unit", "--directory", "--after-cursor"}},
		{"from the beginning", Reader{MaxPriority: 7, FromBeginning: true}, "", []string{"--lines=all"}, []string{"--lines=0"}},
		{"a cursor wins over from beginning", Reader{MaxPriority: 7, FromBeginning: true}, "s=abc;i=1", []string{"--after-cursor=s=abc;i=1"}, []string{"--lines"}},
		{"units, priority and directory", Reader{MaxPriority: 4, Units: []string{"nginx.service", "sshd*"}, Directory: "/mnt/journal"}, "", []string{"--priority=4", "--unit=nginx.service", "--unit=sshd*", "--directory=/mnt/journal"}, nil},
	}

	for _, c := range cases {
		joined := " " + strings.Join(c.reader.args(c.cursor), " ") + " "

		for _, want := range c.want {
			if !strings.Contains(joined, " "+want+" ") {
				t.Errorf("%s: %q missing from %q", c.name, want, joined)
			}
		}

		for _, not := range c.not {
			if strings.Contains(joined, " "+not) {
				t.Errorf("%s: %q should not be in %q", c.name, not, joined)
			}
		}
	}
}

func TestPriorityNames(t *testing.T) {
	for name, want := range map[string]int{"": 7, "debug": 7, "info": 6, "Notice": 5, "warning": 4, "warn": 4, "error": 3, "err": 3, "critical": 2, "alert": 1, "emergency": 0} {
		if got, ok := Priority(name); !ok || got != want {
			t.Errorf("%q: got %d %v, want %d", name, got, ok, want)
		}
	}

	if _, ok := Priority("loud"); ok {
		t.Error("an unknown name should be refused")
	}
}

func TestAnEntryBecomesALogEntry(t *testing.T) {
	reader := &Reader{Host: "fallback"}

	event, ok := reader.entry([]byte(entryJSON("s=1;i=2", map[string]any{
		"MESSAGE":              "Started nginx",
		"PRIORITY":             "6",
		"__REALTIME_TIMESTAMP": "1761561600123456",
		"_HOSTNAME":            "web-1",
		"_SYSTEMD_UNIT":        "nginx.service",
		"SYSLOG_IDENTIFIER":    []string{"systemd", "other"},
		"_PID":                 "1",
	})))
	if !ok || event.Cursor != "s=1;i=2" || event.Key != Key {
		t.Fatalf("event %+v %v", event, ok)
	}

	var got map[string]any
	_ = json.Unmarshal(event.Entry, &got)

	journald := got["context"].(map[string]any)["journald"].(map[string]any)

	if got["message"] != "Started nginx" || got["level"] != "info" || got["host"] != "web-1" || got["ts"] != "2025-10-27T10:40:00.123456Z" {
		t.Errorf("entry %v", got)
	}

	if journald["unit"] != "nginx.service" || journald["identifier"] != "systemd" || journald["pid"] != "1" {
		t.Errorf("context %v", journald)
	}
}

func TestAnEntryWithoutAHostTakesTheAgentsHostAndOddInputIsSkipped(t *testing.T) {
	reader := &Reader{Host: "fallback"}

	event, ok := reader.entry([]byte(entryJSON("c", map[string]any{"MESSAGE": "hi", "PRIORITY": "9"})))
	if !ok {
		t.Fatal("entry refused")
	}

	var got map[string]any
	_ = json.Unmarshal(event.Entry, &got)

	if got["host"] != "fallback" || got["level"] != nil {
		t.Errorf("entry %v", got)
	}

	for _, bad := range []string{"not json", `{"MESSAGE":"no cursor"}`, `[]`} {
		if _, ok := reader.entry([]byte(bad)); ok {
			t.Errorf("%q should be skipped", bad)
		}
	}
}

func TestABinaryMessageIsDescribedAndALongOneIsCutOnACharacter(t *testing.T) {
	reader := &Reader{}

	event, _ := reader.entry([]byte(`{"__CURSOR":"c","MESSAGE":[1,2,3,255]}`))

	var got map[string]any
	_ = json.Unmarshal(event.Entry, &got)

	if got["message"] != "[binary message of 4 bytes]" {
		t.Errorf("message %v", got["message"])
	}

	long := strings.Repeat("é", maxMessage)
	event, _ = reader.entry([]byte(entryJSON("c", map[string]any{"MESSAGE": long})))
	_ = json.Unmarshal(event.Entry, &got)

	if message := got["message"].(string); len(message) > maxMessage || strings.ContainsRune(message, '�') {
		t.Errorf("message of %d bytes", len(message))
	}
}

type collector struct {
	mu     sync.Mutex
	events []source.Event
}

func (c *collector) emit(event source.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.events = append(c.events, event)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.events)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if ok() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

func TestRunEmitsEntriesAndStartsAgainAfterTheLastCursor(t *testing.T) {
	record := t.TempDir() + "/args"
	first := entryJSON("c1", map[string]any{"MESSAGE": "one"}) + entryJSON("c2", map[string]any{"MESSAGE": "two"})
	second := entryJSON("c3", map[string]any{"MESSAGE": "three"})

	events := &collector{}
	reader := &Reader{
		MaxPriority: 7,
		Emit:        events.emit,
		RetryAfter:  10 * time.Millisecond,
		Logf:        t.Logf,
		Command:     helper("JOURNALD_HELPER_ARGS="+record, "JOURNALD_HELPER_OUTPUT="+first, "JOURNALD_HELPER_OUTPUT_AFTER="+second, "JOURNALD_HELPER_EXIT=1"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		reader.Run(ctx)
		close(done)
	}()

	waitFor(t, "three entries", func() bool { return events.count() >= 3 })
	cancel()
	<-done

	events.mu.Lock()
	defer events.mu.Unlock()

	if events.events[0].Cursor != "c1" || events.events[1].Cursor != "c2" || events.events[2].Cursor != "c3" {
		t.Fatalf("events %+v", events.events[:3])
	}

	raw, _ := os.ReadFile(record)
	starts := strings.Split(strings.TrimSpace(string(raw)), "\n")

	if !strings.Contains(starts[0], "--lines=0") || !strings.Contains(starts[1], "--after-cursor=c2") {
		t.Errorf("starts %q", starts)
	}
}

func TestRunResumesFromTheSavedCursor(t *testing.T) {
	record := t.TempDir() + "/args"
	events := &collector{}
	reader := &Reader{
		MaxPriority: 7,
		Cursor:      "saved",
		Emit:        events.emit,
		Command:     helper("JOURNALD_HELPER_ARGS="+record, "JOURNALD_HELPER_OUTPUT_AFTER="+entryJSON("c9", map[string]any{"MESSAGE": "later"})),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		reader.Run(ctx)
		close(done)
	}()

	waitFor(t, "an entry", func() bool { return events.count() >= 1 })
	cancel()
	<-done

	raw, _ := os.ReadFile(record)
	if !strings.Contains(string(raw), "--after-cursor=saved") {
		t.Errorf("args %q", raw)
	}
}

func TestAMissingJournalctlIsRetriedAndStopsWithTheContext(t *testing.T) {
	reader := &Reader{Path: "/nonexistent/journalctl", MaxPriority: 7, RetryAfter: 5 * time.Millisecond, Logf: t.Logf}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})

	go func() {
		reader.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
}

func TestAnOverlongEntryIsSkippedWithoutStoppingTheStream(t *testing.T) {
	huge := `{"__CURSOR":"big","MESSAGE":"` + strings.Repeat("x", maxLine+10) + `"}` + "\n"
	events := &collector{}
	reader := &Reader{Emit: events.emit}

	last := reader.read(strings.NewReader(huge + entryJSON("after", map[string]any{"MESSAGE": "ok"})))

	if last != "after" || events.count() != 1 {
		t.Fatalf("last %q, %d events", last, events.count())
	}
}
