package tail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const stamp = "2026-10-27T10:00:00.123456789Z"

func cri(stream, flag, text string) string {
	return stamp + " " + stream + " " + flag + " " + text + "\n"
}

func TestDecodeCRI(t *testing.T) {
	now := time.Now()

	for name, tc := range map[string]struct {
		format, line string
		text         string
		stream       string
		partial      bool
		timed        bool
	}{
		"a full line":          {FormatCRI, stamp + " stdout F hello world", "hello world", "stdout", false, true},
		"a piece":              {FormatCRI, stamp + " stderr P part of a long", "part of a long", "stderr", true, true},
		"an empty message":     {FormatCRI, stamp + " stdout F", "", "stdout", false, true},
		"spaces kept":          {FormatCRI, stamp + " stdout F  indented  text ", " indented  text ", "stdout", false, true},
		"not cri passes on":    {FormatCRI, "just text", "just text", "", false, false},
		"bad time passes on":   {FormatCRI, "yesterday stdout F x", "yesterday stdout F x", "", false, false},
		"bad stream passes on": {FormatCRI, stamp + " other F x", stamp + " other F x", "", false, false},
		"bad flag passes on":   {FormatCRI, stamp + " stdout X x", stamp + " stdout X x", "", false, false},
		"docker line":          {FormatDocker, `{"log":"hi\n","stream":"stderr","time":"` + stamp + `"}`, "hi", "stderr", false, true},
		"docker piece":         {FormatDocker, `{"log":"half","stream":"stdout","time":"` + stamp + `"}`, "half", "stdout", true, true},
		"docker not json":      {FormatDocker, "text", "text", "", false, false},
		"auto picks cri":       {FormatAuto, stamp + " stdout F auto", "auto", "stdout", false, true},
		"auto picks docker":    {FormatAuto, `{"log":"d\n","stream":"stdout","time":"` + stamp + `"}`, "d", "stdout", false, true},
		"auto passes on json":  {FormatAuto, `{"level":"info"}`, `{"level":"info"}`, "", false, false},
		"raw is left alone":    {FormatRaw, stamp + " stdout F x", stamp + " stdout F x", "", false, false},
	} {
		got := decode(tc.format, tc.line, now)

		if got.text != tc.text || got.stream != tc.stream || got.partial != tc.partial {
			t.Errorf("%s: got %+v", name, got)
		}

		if tc.timed && got.time.Format(time.RFC3339Nano) != stamp {
			t.Errorf("%s: time %v", name, got.time)
		}

		if !tc.timed && !got.time.Equal(now) {
			t.Errorf("%s: should keep the time it was read at, got %v", name, got.time)
		}
	}
}

func podFile(t *testing.T, root, name string) string {
	t.Helper()

	dir := filepath.Join(root, "shop_cart-7d9f_0a1b2c3d-0000-0000-0000-000000000001", "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	return filepath.Join(dir, name)
}

func criSource(root string) Source {
	return Source{Pattern: filepath.Join(root, "*", "*", "*.log"), Format: FormatCRI, FromBeginning: true}
}

func TestACriFileIsFollowedWithItsTimesAndStreams(t *testing.T) {
	root := t.TempDir()
	path := podFile(t, root, "0.log")
	appendTo(t, path, cri("stdout", "F", "first")+cri("stderr", "F", "second"))

	lines, _ := start(t, &Follower{Sources: []Source{criSource(root)}})

	first, second := next(t, lines), next(t, lines)

	if first.Text != "first" || first.Stream != "stdout" || second.Text != "second" || second.Stream != "stderr" {
		t.Fatalf("got %+v and %+v", first, second)
	}

	if first.Time.Format(time.RFC3339Nano) != stamp {
		t.Errorf("a line takes the runtime's time, got %v", first.Time)
	}

	if first.Offset >= second.Offset {
		t.Errorf("offsets must grow: %d then %d", first.Offset, second.Offset)
	}
}

func TestThePiecesOfALongLineAreJoinedAndTheOffsetIsThatOfTheLastPiece(t *testing.T) {
	root := t.TempDir()
	path := podFile(t, root, "0.log")
	text := cri("stdout", "P", "aaa") + cri("stdout", "P", "bbb") + cri("stdout", "F", "ccc") + cri("stdout", "F", "next")
	appendTo(t, path, text)

	lines, _ := start(t, &Follower{Sources: []Source{criSource(root)}})

	whole := next(t, lines)
	size := int64(len(cri("stdout", "P", "aaa")) + len(cri("stdout", "P", "bbb")) + len(cri("stdout", "F", "ccc")))

	if whole.Text != "aaabbbccc" || whole.Offset != size {
		t.Fatalf("got %q at %d, want aaabbbccc at %d", whole.Text, whole.Offset, size)
	}

	if next(t, lines).Text != "next" {
		t.Fatal("the line after a joined one is its own")
	}
}

func TestAnUnfinishedLongLineIsSentAfterAWhile(t *testing.T) {
	root := t.TempDir()
	path := podFile(t, root, "0.log")
	appendTo(t, path, cri("stdout", "P", "never finished"))

	lines, _ := start(t, &Follower{Sources: []Source{criSource(root)}, PartialAfter: 50 * time.Millisecond})

	if got := next(t, lines); got.Text != "never finished" {
		t.Fatalf("got %q", got.Text)
	}
}

func TestADockerJSONFileIsDecoded(t *testing.T) {
	root := t.TempDir()
	path := podFile(t, root, "0.log")
	appendTo(t, path, `{"log":"one\n","stream":"stdout","time":"`+stamp+`"}`+"\n"+`{"log":"two\n","stream":"stderr","time":"`+stamp+`"}`+"\n")

	source := criSource(root)
	source.Format = FormatDocker
	lines, _ := start(t, &Follower{Sources: []Source{source}})

	if a, b := next(t, lines), next(t, lines); a.Text != "one" || b.Text != "two" || b.Stream != "stderr" {
		t.Fatalf("got %+v %+v", a, b)
	}
}

func TestAPodThatRestartsMidReadKeepsEveryLineOnceAndInOrder(t *testing.T) {
	root := t.TempDir()
	first := podFile(t, root, "0.log")
	appendTo(t, first, cri("stdout", "F", "boot 1"))

	lines, _ := start(t, &Follower{Sources: []Source{criSource(root)}})

	if next(t, lines).Text != "boot 1" {
		t.Fatal("first line missing")
	}

	// The container dies with a line half written, restarts, and the kubelet starts 1.log.
	appendTo(t, first, cri("stderr", "F", "panic: boom"))
	appendTo(t, podFile(t, root, "1.log"), cri("stdout", "F", "boot 2")+cri("stdout", "F", "ready"))

	got := map[string][]string{}

	for range 3 {
		line := next(t, lines)
		got[filepath.Base(line.File)] = append(got[filepath.Base(line.File)], line.Text)
	}

	if strings.Join(got["0.log"], "|") != "panic: boom" || strings.Join(got["1.log"], "|") != "boot 2|ready" {
		t.Fatalf("got %v", got)
	}

	select {
	case extra := <-lines:
		t.Fatalf("a line arrived twice or from nowhere: %+v", extra)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestTheKubeletRotatingAFileLosesAndRepeatsNothing(t *testing.T) {
	root := t.TempDir()
	path := podFile(t, root, "0.log")
	appendTo(t, path, cri("stdout", "F", "one")+cri("stdout", "F", "two"))

	lines, _ := start(t, &Follower{Sources: []Source{criSource(root)}})

	if next(t, lines).Text != "one" || next(t, lines).Text != "two" {
		t.Fatal("missing lines")
	}

	// A line is written, then the file is renamed away (0.log.<time>), which a glob for N.log no
	// longer matches, and a new 0.log is created.
	appendTo(t, path, cri("stdout", "F", "three"))

	if err := os.Rename(path, path+".20261027-101500"); err != nil {
		t.Fatal(err)
	}

	appendTo(t, path, cri("stdout", "F", "four"))

	if a, b := next(t, lines), next(t, lines); a.Text != "three" || b.Text != "four" {
		t.Fatalf("got %q then %q", a.Text, b.Text)
	}

	select {
	case extra := <-lines:
		t.Fatalf("the rotated file was read again: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestARotatedFileThatTheGlobDoesNotMatchIsNeverFollowed(t *testing.T) {
	root := t.TempDir()
	appendTo(t, podFile(t, root, "0.log.20261027-101500"), cri("stdout", "F", "old news"))
	appendTo(t, podFile(t, root, "0.log.20261027-101500.gz"), "binary")

	lines, _ := start(t, &Follower{Sources: []Source{criSource(root)}})

	select {
	case got := <-lines:
		t.Fatalf("followed a rotated file: %+v", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestADeletedPodIsReportedGoneAfterItsLastLines(t *testing.T) {
	root := t.TempDir()
	path := podFile(t, root, "0.log")
	appendTo(t, path, cri("stdout", "F", "last words"))

	gone := make(chan string, 1)
	lines, _ := start(t, &Follower{Sources: []Source{criSource(root)}, Gone: func(path string) { gone <- path }})

	if next(t, lines).Text != "last words" {
		t.Fatal("missing line")
	}

	appendTo(t, path, cri("stdout", "F", "goodbye"))

	if err := os.RemoveAll(filepath.Dir(filepath.Dir(path))); err != nil {
		t.Fatal(err)
	}

	if got := next(t, lines); got.Text != "goodbye" {
		t.Fatalf("the lines written before the pod went must still be sent, got %q", got.Text)
	}

	select {
	case reported := <-gone:
		if reported != path {
			t.Errorf("reported %q", reported)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the removed file was not reported")
	}
}

func TestAcceptSkipsFilesAndTheirNewcomers(t *testing.T) {
	root := t.TempDir()
	appendTo(t, podFile(t, root, "0.log"), cri("stdout", "F", "kept"))

	source := criSource(root)
	source.Accept = func(path string) bool { return !strings.Contains(path, "skipme") }

	skipped := filepath.Join(root, "kube-system_skipme-1_0a1b2c3d-0000-0000-0000-000000000002", "x")
	if err := os.MkdirAll(skipped, 0o755); err != nil {
		t.Fatal(err)
	}

	appendTo(t, filepath.Join(skipped, "0.log"), cri("stdout", "F", "secret"))

	lines, _ := start(t, &Follower{Sources: []Source{source}})

	if got := next(t, lines); got.Text != "kept" {
		t.Fatalf("got %q", got.Text)
	}

	select {
	case got := <-lines:
		t.Fatalf("a refused file was followed: %+v", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestATruncatedContainerFileStartsOverWithoutAGhost(t *testing.T) {
	root := t.TempDir()
	path := podFile(t, root, "0.log")
	appendTo(t, path, cri("stdout", "P", "half")+cri("stdout", "F", "whole"))

	lines, _ := start(t, &Follower{Sources: []Source{criSource(root)}})

	if next(t, lines).Text != "halfwhole" {
		t.Fatal("missing line")
	}

	appendTo(t, path, cri("stdout", "P", "stale piece"))
	time.Sleep(60 * time.Millisecond)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}

	appendTo(t, path, cri("stdout", "F", "fresh"))

	if got := next(t, lines).Text; got != "fresh" {
		t.Fatalf("a piece from before the truncation was joined to the new file: %q", got)
	}
}
