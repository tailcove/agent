package tail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func start(t *testing.T, follower *Follower) (<-chan Line, context.CancelFunc) {
	t.Helper()

	lines := make(chan Line, 100)
	follower.Lines = lines
	follower.PollInterval = 10 * time.Millisecond
	follower.RescanInterval = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		follower.Run(ctx)
		close(done)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return lines, cancel
}

func next(t *testing.T, lines <-chan Line) Line {
	t.Helper()

	select {
	case line := <-lines:
		return line
	case <-time.After(3 * time.Second):
		t.Fatal("no line arrived")

		return Line{}
	}
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()

	handle, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	defer handle.Close()

	if _, err := handle.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyNewLinesAreSentForAFileThatWasAlreadyThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "old line\n")

	lines, _ := start(t, &Follower{Sources: []Source{{Pattern: path}}})
	time.Sleep(60 * time.Millisecond)
	appendTo(t, path, "new line\n")

	if line := next(t, lines); line.Text != "new line" {
		t.Fatalf("got %q", line.Text)
	}
}

func TestFromBeginningReadsTheWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "one\ntwo\n")

	lines, _ := start(t, &Follower{Sources: []Source{{Pattern: path, FromBeginning: true}}})

	first, second := next(t, lines), next(t, lines)
	if first.Text != "one" || second.Text != "two" || second.Offset != int64(len("one\ntwo\n")) {
		t.Fatalf("got %+v %+v", first, second)
	}
}

func TestAPartialLineWaitsForItsNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "")

	lines, _ := start(t, &Follower{Sources: []Source{{Pattern: path}}})
	time.Sleep(60 * time.Millisecond)
	appendTo(t, path, "half a li")
	time.Sleep(80 * time.Millisecond)

	select {
	case line := <-lines:
		t.Fatalf("sent a partial line: %q", line.Text)
	default:
	}

	appendTo(t, path, "ne\n")

	if line := next(t, lines); line.Text != "half a line" {
		t.Fatalf("got %q", line.Text)
	}
}

func TestRotationFinishesTheOldFileThenReadsTheNewOne(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "app.log")
	appendTo(t, path, "")

	lines, _ := start(t, &Follower{Sources: []Source{{Pattern: path}}})
	time.Sleep(50 * time.Millisecond)

	appendTo(t, path, "before rotation\n")

	if line := next(t, lines); line.Text != "before rotation" {
		t.Fatalf("got %q", line.Text)
	}

	appendTo(t, path, "written just before the rename\n")

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}

	appendTo(t, path, "after rotation\n")

	got := []string{next(t, lines).Text, next(t, lines).Text}
	if got[0] != "written just before the rename" || got[1] != "after rotation" {
		t.Fatalf("got %q", got)
	}
}

func TestTruncationRestartsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "")

	lines, _ := start(t, &Follower{Sources: []Source{{Pattern: path}}})
	time.Sleep(50 * time.Millisecond)
	appendTo(t, path, "a long first line that will be cut away\n")
	next(t, lines)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}

	appendTo(t, path, "fresh\n")

	if line := next(t, lines); line.Text != "fresh" {
		t.Fatalf("got %q", line.Text)
	}
}

func TestAGlobPicksUpFilesCreatedLater(t *testing.T) {
	directory := t.TempDir()

	lines, _ := start(t, &Follower{Sources: []Source{{Pattern: filepath.Join(directory, "*.log")}}})
	time.Sleep(60 * time.Millisecond)
	appendTo(t, filepath.Join(directory, "late.log"), "hello from a new file\n")

	if line := next(t, lines); line.Text != "hello from a new file" || !strings.HasSuffix(line.File, "late.log") {
		t.Fatalf("got %+v", line)
	}
}

func TestResumeContinuesWhereTheLastRunStopped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "seen\nunseen\n")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	resume := map[string]Position{path: {Inode: inodeOf(info), Offset: int64(len("seen\n"))}}
	lines, _ := start(t, &Follower{Sources: []Source{{Pattern: path}}, Resume: resume})

	if line := next(t, lines); line.Text != "unseen" {
		t.Fatalf("got %q", line.Text)
	}
}

func TestAVeryLongLineIsCutAndMarked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "")

	lines, _ := start(t, &Follower{Sources: []Source{{Pattern: path}}})
	time.Sleep(50 * time.Millisecond)
	appendTo(t, path, strings.Repeat("x", 300*1024)+"\nnext\n")

	long := next(t, lines)
	if len(long.Text) > MaxLineBytes+len(truncationMark) || !strings.HasSuffix(long.Text, "[truncated]") {
		t.Fatalf("line of %d bytes not cut: ...%q", len(long.Text), long.Text[len(long.Text)-20:])
	}

	if line := next(t, lines); line.Text != "next" {
		t.Fatalf("got %q after the long line", line.Text)
	}
}
