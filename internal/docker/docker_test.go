package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tailcove/agent/internal/source"
	"github.com/tailcove/agent/internal/tail"
)

const (
	id1 = "aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
	id2 = "bbbbbbbbbbbb2222222222222222222222222222222222222222222222222222"
)

func frame(stream byte, text string) []byte {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(text)))

	return append(header, text...)
}

func stamped(at, text string) string { return at + " " + text + "\n" }

func TestDemuxReadsFramesAndStopsAtTheEnd(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(frame(1, "out one\n"))
	stream.Write(frame(2, "err one\n"))

	var got []Chunk
	if err := Demux(&stream, false, func(c Chunk) { got = append(got, c) }); err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0].Stream != "stdout" || got[1].Stream != "stderr" || string(got[1].Data) != "err one\n" {
		t.Fatalf("got %+v", got)
	}
}

func TestDemuxRefusesAStreamThatIsNotFramed(t *testing.T) {
	for name, bad := range map[string][]byte{
		"plain text":   []byte("2026-10-27T10:00:00Z hello\n"),
		"unknown kind": append([]byte{9, 0, 0, 0, 0, 0, 0, 1}, 'x'),
		"huge frame":   {1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff},
	} {
		if err := Demux(bytes.NewReader(bad), false, func(Chunk) {}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestDemuxOfATerminalIsAllStdout(t *testing.T) {
	var got strings.Builder

	_ = Demux(strings.NewReader("a b c"), true, func(c Chunk) {
		if c.Stream != "stdout" {
			t.Errorf("stream %s", c.Stream)
		}

		got.Write(c.Data)
	})

	if got.String() != "a b c" {
		t.Fatalf("got %q", got.String())
	}
}

func TestLinesAreCompletedAcrossFrames(t *testing.T) {
	lines := NewLines()

	if got := lines.Add(Chunk{Stream: "stdout", Data: []byte("2026-10-27T10:00:00.5Z hel")}); len(got) != 0 {
		t.Fatalf("early lines %+v", got)
	}

	got := lines.Add(Chunk{Stream: "stdout", Data: []byte("lo\n2026-10-27T10:00:01Z second\r\n")})
	if len(got) != 2 || got[0].Text != "hello" || got[1].Text != "second" || got[0].Time.Nanosecond() != 500000000 {
		t.Fatalf("got %+v", got)
	}

	// A partial line on one stream is not mixed with the other.
	lines.Add(Chunk{Stream: "stdout", Data: []byte("2026-10-27T10:00:02Z out")})

	if got := lines.Add(Chunk{Stream: "stderr", Data: []byte("2026-10-27T10:00:02Z err\n")}); len(got) != 1 || got[0].Text != "err" {
		t.Fatalf("got %+v", got)
	}
}

func TestALineWithoutATimeIsDroppedAndAnEndlessOneIsCut(t *testing.T) {
	lines := NewLines()

	if got := lines.Add(Chunk{Stream: "stdout", Data: []byte("no time here\n")}); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}

	long := "2026-10-27T10:00:00Z " + strings.Repeat("x", maxLine*2+10)
	first := lines.Add(Chunk{Stream: "stdout", Data: []byte(long)})

	if len(first) == 0 || len(first[0].Text) > maxLine {
		t.Fatalf("not cut: %d lines", len(first))
	}

	rest := lines.Add(Chunk{Stream: "stdout", Data: []byte("tail\n")})
	if len(rest) != 1 || !strings.HasSuffix(rest[0].Text, "tail") || rest[0].Time.IsZero() {
		t.Fatalf("rest %+v", rest)
	}
}

// engine is a Docker daemon on a unix socket that serves what the tests give it.
type engine struct {
	mu       sync.Mutex
	methods  []string
	paths    []string
	listing  string
	tty      bool
	logs     []byte
	logQuery string
	status   int
}

func (e *engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.methods = append(e.methods, r.Method)
	e.paths = append(e.paths, r.URL.Path)
	e.mu.Unlock()

	if e.status != 0 {
		http.Error(w, "boom", e.status)

		return
	}

	switch {
	case r.URL.Path == "/containers/json":
		_, _ = io.WriteString(w, e.listing)
	case strings.HasSuffix(r.URL.Path, "/json"):
		_ = json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Tty": e.tty}})
	case strings.HasSuffix(r.URL.Path, "/logs"):
		e.mu.Lock()
		e.logQuery = r.URL.RawQuery
		e.mu.Unlock()

		_, _ = w.Write(e.logs)
	default:
		http.NotFound(w, r)
	}
}

func startEngine(t *testing.T, e *engine) *Client {
	t.Helper()

	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "d.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets are not available: %v", err)
	}

	server := httptest.NewUnstartedServer(e)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	return NewClient(socket)
}

func TestClientListsInspectsAndReadsLogsWithGetOnly(t *testing.T) {
	e := &engine{
		listing: `[{"Id":"` + id1 + `","Names":["/web-1"],"Image":"nginx:1","Labels":{"com.docker.compose.service":"web"}},{"Id":"not-an-id","Names":["/x"]}]`,
		tty:     true,
		logs:    frame(1, stamped("2026-10-27T10:00:00Z", "hi")),
	}
	client := startEngine(t, e)

	containers, err := client.List(context.Background())
	if err != nil || len(containers) != 1 || containers[0].Name != "web-1" || containers[0].Image != "nginx:1" {
		t.Fatalf("containers %+v %v", containers, err)
	}

	if tty, err := client.Tty(context.Background(), id1); err != nil || !tty {
		t.Fatalf("tty %v %v", tty, err)
	}

	stream, err := client.Logs(context.Background(), id1, time.Unix(1761561600, 5))
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(stream)
	_ = stream.Close()

	if !bytes.Equal(body, e.logs) {
		t.Fatalf("body %q", body)
	}

	for _, want := range []string{"follow=1", "stdout=1", "stderr=1", "timestamps=1", "since=1761561600.000000005"} {
		if !strings.Contains(e.logQuery, want) {
			t.Errorf("%q missing from %q", want, e.logQuery)
		}
	}

	for _, method := range e.methods {
		if method != http.MethodGet {
			t.Errorf("the client made a %s request", method)
		}
	}
}

func TestClientRefusesAnythingButAContainerIdAndReportsEngineErrors(t *testing.T) {
	e := &engine{status: http.StatusInternalServerError}
	client := startEngine(t, e)

	for _, bad := range []string{"", "../../images", "web-1", strings.Repeat("g", 64), "abc"} {
		if _, err := client.Logs(context.Background(), bad, time.Time{}); err == nil {
			t.Errorf("%q was accepted by Logs", bad)
		}

		if _, err := client.Tty(context.Background(), bad); err == nil {
			t.Errorf("%q was accepted by Tty", bad)
		}
	}

	if len(e.paths) != 0 {
		t.Errorf("a refused id reached the engine: %v", e.paths)
	}

	if _, err := client.List(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("error %v", err)
	}
}

func TestSinceIsTheEnginesSecondsAndNanoseconds(t *testing.T) {
	if got := sinceParameter(time.Time{}); got != "0" {
		t.Errorf("zero: %q", got)
	}

	if got := sinceParameter(time.Unix(10, 7)); got != "10.000000007" {
		t.Errorf("got %q", got)
	}
}

// fakeEngine serves reader tests without a socket.
type fakeEngine struct {
	mu         sync.Mutex
	containers []Container
	logs       map[string]string // id -> framed stream, delivered then held open
	sinces     map[string]time.Time
	closers    []*holdOpen
}

type holdOpen struct {
	io.Reader
	once   sync.Once
	closed chan struct{}
}

func (h *holdOpen) Read(p []byte) (int, error) {
	n, err := h.Reader.Read(p)
	if err == io.EOF {
		<-h.closed

		return 0, io.EOF
	}

	return n, err
}

func (h *holdOpen) Close() error {
	h.once.Do(func() { close(h.closed) })

	return nil
}

func (f *fakeEngine) List(context.Context) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]Container(nil), f.containers...), nil
}

func (f *fakeEngine) Tty(context.Context, string) (bool, error) { return false, nil }

func (f *fakeEngine) Logs(_ context.Context, id string, since time.Time) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.sinces == nil {
		f.sinces = map[string]time.Time{}
	}

	f.sinces[id] = since
	holder := &holdOpen{Reader: strings.NewReader(f.logs[id]), closed: make(chan struct{})}
	f.closers = append(f.closers, holder)

	return holder, nil
}

type sink struct {
	mu     sync.Mutex
	events []source.Event
}

func (s *sink) emit(e source.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.events = append(s.events, e)
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.events)
}

func (s *sink) object(t *testing.T, i int) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	var out map[string]any
	if err := json.Unmarshal(s.events[i].Entry, &out); err != nil {
		t.Fatal(err)
	}

	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if ok() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

func runReader(t *testing.T, r *Reader) (stop func()) {
	t.Helper()

	r.Interval = 20 * time.Millisecond
	r.Logf = t.Logf

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		r.Run(ctx)
		close(done)
	}()

	return func() {
		cancel()
		<-done
	}
}

func TestReaderFollowsAContainerAndMapsItsLabels(t *testing.T) {
	web := Container{ID: id1, Name: "shop-web-1", Image: "shop/web:3", Labels: map[string]string{composeProject: "shop", composeService: "web", "team": "payments", "secret": "x"}}
	engine := &fakeEngine{containers: []Container{web}, logs: map[string]string{
		id1: string(frame(1, stamped("2026-10-27T10:00:00.5Z", "starting")+stamped("2026-10-27T10:00:01Z", `{"message":"paid","level":"info"}`))) +
			string(frame(2, stamped("2026-10-27T10:00:02Z", "warn: slow"))),
	}}
	out := &sink{}
	reader := &Reader{Client: engine, Host: "node-1", Labels: []string{"team"}, FromBeginning: true, Emit: out.emit, Now: time.Now}

	defer runReader(t, reader)()

	waitFor(t, "three entries", func() bool { return out.count() >= 3 })

	first := out.object(t, 0)
	docker := first["context"].(map[string]any)["docker"].(map[string]any)

	if first["message"] != "starting" || first["host"] != "shop-web-1" || docker["compose_service"] != "web" || docker["compose_project"] != "shop" || docker["id"] != id1[:12] || docker["image"] != "shop/web:3" {
		t.Errorf("entry %v", first)
	}

	if labels := docker["labels"].(map[string]any); labels["team"] != "payments" || labels["secret"] != nil {
		t.Errorf("labels %v", labels)
	}

	if second := out.object(t, 1); second["message"] != "paid" || second["level"] != "info" || second["host"] != "shop-web-1" || second["ts"] == nil {
		t.Errorf("JSON line %v", second)
	}

	if third := out.object(t, 2); third["context"].(map[string]any)["docker"].(map[string]any)["stream"] != "stderr" {
		t.Errorf("stderr %v", third)
	}

	if out.events[0].Key != KeyPrefix+id1 || out.events[0].Cursor != "2026-10-27T10:00:00.5Z" {
		t.Errorf("key/cursor %+v", out.events[0])
	}
}

func TestReaderHostModes(t *testing.T) {
	c := Container{ID: id1, Name: "web-1", Labels: map[string]string{composeService: "web"}}

	for mode, want := range map[string]string{"": "web-1", "container": "web-1", "host": "node-1", "service": "web"} {
		reader := &Reader{Host: "node-1", HostMode: mode}
		if got := reader.hostFor(c); got != want {
			t.Errorf("%q: got %q, want %q", mode, got, want)
		}
	}
}

func TestReaderStartsAtNowWithoutACursorAndAtTheCursorWithOne(t *testing.T) {
	engine := &fakeEngine{
		containers: []Container{{ID: id1, Name: "a"}, {ID: id2, Name: "b"}},
		logs: map[string]string{
			id1: string(frame(1, stamped("2026-10-27T10:00:00Z", "old")+stamped("2026-10-27T10:00:05Z", "new"))),
			id2: string(frame(1, stamped("2026-10-27T10:00:05Z", "fresh"))),
		},
	}
	out := &sink{}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	reader := &Reader{
		Client: engine, Emit: out.emit, Now: func() time.Time { return now },
		Resume: func(key string) string {
			if key == KeyPrefix+id1 {
				return "2026-10-27T10:00:00Z"
			}

			return ""
		},
	}

	defer runReader(t, reader)()

	// The engine is asked from the cursor for one container and from now for the other.
	waitFor(t, "both containers' new lines", func() bool { return out.count() >= 2 })

	engine.mu.Lock()
	defer engine.mu.Unlock()

	if !engine.sinces[id1].Equal(time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)) || !engine.sinces[id2].Equal(now) {
		t.Errorf("sinces %v", engine.sinces)
	}

	// The line at the cursor itself was delivered before the restart and is not sent again.
	messages := map[any]bool{}

	for i := 0; i < out.count(); i++ {
		messages[out.object(t, i)["message"]] = true
	}

	if messages["old"] || !messages["new"] || !messages["fresh"] {
		t.Errorf("messages %v", messages)
	}
}

func TestReaderJoinsStackTracesAndNeverMovesTheCursorPastWhatIsPending(t *testing.T) {
	multiline, _ := tail.NewMultiline(tail.MultilineAuto, "", 0, 0, 50*time.Millisecond)
	engine := &fakeEngine{
		containers: []Container{{ID: id1, Name: "app"}},
		logs: map[string]string{id1: string(frame(2, stamped("2026-10-27T10:00:01Z", "Exception: boom"))) +
			string(frame(1, stamped("2026-10-27T10:00:02Z", "an unrelated stdout line"))) +
			string(frame(2, stamped("2026-10-27T10:00:03Z", "    at one()"))) +
			string(frame(1, stamped("2026-10-27T10:00:04Z", "another stdout line")))},
	}
	out := &sink{}
	reader := &Reader{Client: engine, FromBeginning: true, Multiline: multiline, Emit: out.emit, Now: time.Now}

	defer runReader(t, reader)()

	waitFor(t, "three events", func() bool { return out.count() >= 3 })

	out.mu.Lock()
	defer out.mu.Unlock()

	var trace *source.Event

	for i := range out.events {
		var entry map[string]any
		_ = json.Unmarshal(out.events[i].Entry, &entry)

		if strings.Contains(entry["message"].(string), "Exception: boom") {
			trace = &out.events[i]

			if entry["message"] != "Exception: boom\n    at one()" {
				t.Errorf("trace %q", entry["message"])
			}
		}
	}

	if trace == nil {
		t.Fatal("the trace was not sent")
	}

	// While the trace was open (from 10:00:01), the stdout line of 10:00:02 was sent
	// with a cursor before the trace's first line, so a crash then would read the trace again.
	if first := out.events[0]; first.Cursor >= "2026-10-27T10:00:01Z" {
		t.Errorf("cursor %s moved past a pending event", first.Cursor)
	}
}

func TestReaderFiltersContainersAndForgetsTheOnesThatAreGone(t *testing.T) {
	engine := &fakeEngine{
		containers: []Container{{ID: id1, Name: "keep"}, {ID: id2, Name: "skip-me"}},
		logs:       map[string]string{id1: string(frame(1, stamped("2026-10-27T10:00:00Z", "a"))), id2: string(frame(1, stamped("2026-10-27T10:00:00Z", "b")))},
	}
	out := &sink{}

	var (
		mu        sync.Mutex
		forgotten []string
	)

	reader := &Reader{
		Client: engine, Exclude: []string{"skip-*"}, FromBeginning: true, Emit: out.emit, Now: time.Now,
		Keys: func() []string { return []string{KeyPrefix + id1, KeyPrefix + "ccccccccccccdead"} },
		Forget: func(key string) {
			mu.Lock()
			defer mu.Unlock()

			forgotten = append(forgotten, key)
		},
	}

	defer runReader(t, reader)()

	waitFor(t, "the kept container", func() bool { return out.count() >= 1 })
	waitFor(t, "the unknown container's cursor to be dropped", func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(forgotten) > 0
	})

	mu.Lock()
	defer mu.Unlock()

	if len(forgotten) != 1 || forgotten[0] != KeyPrefix+"ccccccccccccdead" {
		t.Errorf("forgotten %v", forgotten)
	}

	if out.count() != 1 || out.object(t, 0)["message"] != "a" {
		t.Errorf("entries %d", out.count())
	}
}

func TestAcceptUsesIncludeAndExclude(t *testing.T) {
	reader := &Reader{Include: []string{"web-*", "db"}, Exclude: []string{"web-canary"}}

	for name, want := range map[string]bool{"web-1": true, "db": true, "web-canary": false, "cache": false} {
		if got := reader.Accept(Container{Name: name}); got != want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestReaderStopsStreamsWhenTheContextEnds(t *testing.T) {
	engine := &fakeEngine{containers: []Container{{ID: id1, Name: "a"}}, logs: map[string]string{id1: ""}}
	reader := &Reader{Client: engine, Now: time.Now}

	stop := runReader(t, reader)

	waitFor(t, "the stream to open", func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()

		return len(engine.closers) > 0
	})

	finished := make(chan struct{})

	go func() {
		stop()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader did not stop")
	}
}

func TestTheCursorEndsAtTheLatestLineWhicheverStreamIsSentLast(t *testing.T) {
	multiline, _ := tail.NewMultiline(tail.MultilineAuto, "", 0, 0, 30*time.Millisecond)

	for round := 0; round < 5; round++ {
		engine := &fakeEngine{
			containers: []Container{{ID: id1, Name: "app"}},
			logs: map[string]string{id1: string(frame(1, stamped("2026-10-27T10:00:00Z", "plain line"))) +
				string(frame(2, stamped("2026-10-27T10:00:01Z", "error line")))},
		}
		out := &sink{}
		reader := &Reader{Client: engine, FromBeginning: true, Multiline: multiline, Emit: out.emit, Now: time.Now}

		stop := runReader(t, reader)

		waitFor(t, "both events", func() bool { return out.count() >= 2 })
		stop()

		out.mu.Lock()
		last := out.events[len(out.events)-1].Cursor
		out.mu.Unlock()

		if last != "2026-10-27T10:00:01Z" {
			t.Fatalf("round %d: the last cursor is %s, want the time of the latest line", round, last)
		}
	}
}
