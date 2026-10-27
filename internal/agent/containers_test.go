package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tailcove/agent/internal/config"
	"github.com/tailcove/agent/internal/sender"
)

func startAgent(t *testing.T, server *httptest.Server, cfg config.Config, statePath string) *kubeRun {
	t.Helper()

	if statePath == "" {
		statePath = filepath.Join(t.TempDir(), "state.json")
	}

	s := sender.New(server.URL, "main-token", "agent-test")
	s.MinBackoff, s.MaxBackoff = time.Millisecond, 5*time.Millisecond

	cfg.Token, cfg.Endpoint, cfg.Host, cfg.StateFile = "main-token", server.URL, "node-1", statePath
	cfg.Metrics = config.Metrics{Enabled: &off}

	agent := &Agent{Config: cfg, Sender: s, State: LoadState(statePath), Logf: t.Logf}

	ctx, cancel := context.WithCancel(context.Background())
	run := &kubeRun{agent: agent, state: statePath, cancel: cancel, done: make(chan struct{})}

	go func() {
		agent.Run(ctx)
		close(run.done)
	}()

	// An assertion that fails must not leave the agent running: stopping twice is fine.
	t.Cleanup(run.stop)

	return run
}

func messages(recorded *tokenRecorder) []string {
	var out []string

	for _, entry := range recorded.entries("main-token") {
		out = append(out, fmt.Sprint(entry["message"]))
	}

	return out
}

// fakeJournalctl is a script that prints entries (the second set when it is
// started after a cursor) and then waits like journalctl --follow.
func fakeJournalctl(t *testing.T, first, second string) (path, record string) {
	t.Helper()

	dir := t.TempDir()
	record = filepath.Join(dir, "starts")
	path = filepath.Join(dir, "journalctl")

	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$*" in
*--after-cursor=*) printf '%%s' '%s' ;;
*) printf '%%s' '%s' ;;
esac
exec sleep 60
`, record, second, first)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return path, record
}

func journalLine(cursor, message, priority string) string {
	encoded, _ := json.Marshal(map[string]string{"__CURSOR": cursor, "MESSAGE": message, "PRIORITY": priority, "_SYSTEMD_UNIT": "app.service", "_HOSTNAME": "web-1", "__REALTIME_TIMESTAMP": "1761561600000000"})

	return string(encoded) + "\n"
}

func TestJournaldEntriesAreDeliveredAndTheCursorSurvivesARestart(t *testing.T) {
	first := journalLine("c1", "service started", "6") + journalLine("c2", "disk almost full", "4")
	second := journalLine("c3", "after the restart", "3")
	path, record := fakeJournalctl(t, first, second)

	recorded := newTokenRecorder()
	server := httptest.NewServer(recorded)
	defer server.Close()

	cfg := config.Config{Journald: config.Journald{Enabled: true, Path: path, MinPriority: "debug"}}
	statePath := filepath.Join(t.TempDir(), "state.json")

	run := startAgent(t, server, cfg, statePath)

	waitUntil(t, "two entries", func() bool { return recorded.count("main-token") >= 2 })

	waitUntil(t, "the cursor to be recorded", func() bool { return run.agent.State.Cursor("journald") == "c2" })

	entries := recorded.entries("main-token")

	if entries[0]["message"] != "service started" || entries[0]["level"] != "info" || entries[0]["host"] != "web-1" || entries[1]["level"] != "warning" {
		t.Errorf("entries %v", entries)
	}

	if unit := entries[0]["context"].(map[string]any)["journald"].(map[string]any)["unit"]; unit != "app.service" {
		t.Errorf("unit %v", unit)
	}

	run.stop()

	if got := LoadState(statePath).Cursor("journald"); got != "c2" {
		t.Fatalf("saved cursor %q", got)
	}

	// The restart asks the journal for what comes after the saved cursor.
	again := startAgent(t, server, cfg, statePath)
	defer again.stop()

	waitUntil(t, "the entry after the restart", func() bool { return recorded.count("main-token") >= 3 })

	if got := messages(recorded); strings.Join(got, "|") != "service started|disk almost full|after the restart" {
		t.Errorf("messages %v", got)
	}

	raw, _ := os.ReadFile(record)
	if !strings.Contains(string(raw), "--after-cursor=c2") {
		t.Errorf("journalctl was started as: %s", raw)
	}
}

func TestJournaldPriorityAndUnitsReachJournalctl(t *testing.T) {
	path, record := fakeJournalctl(t, journalLine("c1", "x", "3"), "")

	recorded := newTokenRecorder()
	server := httptest.NewServer(recorded)
	defer server.Close()

	cfg := config.Config{Journald: config.Journald{Enabled: true, Path: path, MinPriority: "warning", Units: []string{"nginx.service"}}}

	run := startAgent(t, server, cfg, "")
	defer run.stop()

	waitUntil(t, "an entry", func() bool { return recorded.count("main-token") >= 1 })

	raw, _ := os.ReadFile(record)
	if !strings.Contains(string(raw), "--priority=4") || !strings.Contains(string(raw), "--unit=nginx.service") {
		t.Errorf("journalctl was started as: %s", raw)
	}
}

// dockerEngine is a daemon on a unix socket with one container.
type dockerEngine struct {
	mu    sync.Mutex
	logs  string
	since []string
}

func (d *dockerEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const id = "dddddddddddd0000000000000000000000000000000000000000000000000000"

	switch {
	case r.URL.Path == "/containers/json":
		_, _ = fmt.Fprintf(w, `[{"Id":%q,"Names":["/shop-api-1"],"Image":"shop/api:2","Labels":{"com.docker.compose.service":"api","com.docker.compose.project":"shop"}}]`, id)
	case strings.HasSuffix(r.URL.Path, "/json"):
		_, _ = w.Write([]byte(`{"Config":{"Tty":false}}`))
	case strings.HasSuffix(r.URL.Path, "/logs"):
		d.mu.Lock()
		d.since = append(d.since, r.URL.Query().Get("since"))
		logs := d.logs
		d.mu.Unlock()

		_, _ = w.Write([]byte(logs))

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		<-r.Context().Done()
	default:
		http.NotFound(w, r)
	}
}

func dockerFrame(stream byte, at, text string) string {
	body := at + " " + text + "\n"
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(body)))

	return string(header) + body
}

func startDockerEngine(t *testing.T, engine *dockerEngine) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "ae")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "d.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets are not available: %v", err)
	}

	server := httptest.NewUnstartedServer(engine)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.CloseClientConnections)
	t.Cleanup(server.Close)

	return socket
}

func TestDockerLogsAreDeliveredWithTheirContainerAndDoNotRepeatAfterARestart(t *testing.T) {
	engine := &dockerEngine{logs: dockerFrame(1, "2026-10-27T10:00:00Z", "listening on :8080") +
		dockerFrame(2, "2026-10-27T10:00:01Z", `{"message":"payment failed","level":"error"}`)}
	socket := startDockerEngine(t, engine)

	recorded := newTokenRecorder()
	server := httptest.NewServer(recorded)
	defer server.Close()

	cfg := config.Config{Docker: config.Docker{Enabled: true, Socket: socket, Host: "service", MaxContainers: 10, FromBeginning: true}}
	statePath := filepath.Join(t.TempDir(), "state.json")

	run := startAgent(t, server, cfg, statePath)

	waitUntil(t, "two entries", func() bool { return recorded.count("main-token") >= 2 })

	// The two streams are joined separately, so which of the two comes first is not defined.
	var plain, structured map[string]any

	for _, entry := range recorded.entries("main-token") {
		if entry["message"] == "listening on :8080" {
			plain = entry
		} else {
			structured = entry
		}
	}

	if plain == nil || structured == nil {
		t.Fatalf("entries %v", recorded.entries("main-token"))
	}

	docker := plain["context"].(map[string]any)["docker"].(map[string]any)

	if plain["host"] != "api" || docker["container"] != "shop-api-1" || docker["compose_project"] != "shop" || docker["image"] != "shop/api:2" || docker["stream"] != "stdout" {
		t.Errorf("entry %v", plain)
	}

	if structured["message"] != "payment failed" || structured["level"] != "error" || structured["host"] != "api" {
		t.Errorf("JSON entry %v", structured)
	}

	key := "docker:dddddddddddd0000000000000000000000000000000000000000000000000000"

	waitUntil(t, "the cursor to be recorded", func() bool { return run.agent.State.Cursor(key) == "2026-10-27T10:00:01Z" })
	run.stop()

	if got := LoadState(statePath).Cursor(key); got != "2026-10-27T10:00:01Z" {
		t.Fatalf("saved cursor %q", got)
	}

	// After the restart the engine is asked from the cursor, and a line it repeats is not sent twice.
	engine.mu.Lock()
	engine.logs += dockerFrame(1, "2026-10-27T10:00:02Z", "after the restart")
	engine.mu.Unlock()

	again := startAgent(t, server, cfg, statePath)
	defer again.stop()

	waitUntil(t, "the new line", func() bool { return recorded.count("main-token") >= 3 })
	time.Sleep(200 * time.Millisecond)

	// Nothing was sent twice and nothing was lost. The two streams of one container are joined separately,
	// so the order between them is not defined; the line written after the restart comes last.
	got := messages(recorded)
	if len(got) != 3 || got[2] != "after the restart" {
		t.Errorf("messages %v", got)
	}

	sort.Strings(got)

	if strings.Join(got, "|") != "after the restart|listening on :8080|payment failed" {
		t.Errorf("messages %v", got)
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()

	want := fmt.Sprintf("%d.000000000", time.Date(2026, 10, 27, 10, 0, 1, 0, time.UTC).Unix())

	if last := engine.since[len(engine.since)-1]; last != want {
		t.Errorf("the restart asked for since=%s", last)
	}
}

func TestAnUnreachableDockerSocketDoesNotStopTheAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	recorded := newTokenRecorder()
	server := httptest.NewServer(recorded)
	defer server.Close()

	cfg := config.Config{
		Logs:   []config.LogSource{{Path: path}},
		Docker: config.Docker{Enabled: true, Socket: "/nonexistent/docker.sock", Host: "container", MaxContainers: 10},
	}

	run := startAgent(t, server, cfg, "")
	defer run.stop()

	time.Sleep(300 * time.Millisecond)
	appendTo(t, path, "a file line\n")

	waitUntil(t, "the file line", func() bool { return recorded.count("main-token") >= 1 })
}

func TestTheStateFileOfAnOlderAgentIsStillRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	_ = os.WriteFile(path, []byte(`{"/var/log/a.log":{"Inode":7,"Offset":42}}`), 0o600)

	state := LoadState(path)

	if got := state.Positions()["/var/log/a.log"]; got.Inode != 7 || got.Offset != 42 {
		t.Fatalf("legacy position %+v", got)
	}

	state.RecordCursor("journald", "c9")

	if err := state.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded := LoadState(path)
	if reloaded.Positions()["/var/log/a.log"].Offset != 42 || reloaded.Cursor("journald") != "c9" {
		t.Fatalf("reloaded %+v %q", reloaded.Positions(), reloaded.Cursor("journald"))
	}

	reloaded.ForgetCursor("journald")

	if len(reloaded.CursorKeys("")) != 0 {
		t.Fatal("the cursor was not forgotten")
	}
}
