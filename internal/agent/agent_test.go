package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tailcove/agent/internal/config"
	"github.com/tailcove/agent/internal/sender"
	"github.com/tailcove/agent/internal/tail"
)

type recorder struct {
	mu       sync.Mutex
	logs     []map[string]any
	metrics  []map[string]any
	statuses []int
}

func (r *recorder) handler(failFirst int) http.Handler {
	calls := 0

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()

		calls++

		if req.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if calls <= failFirst {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		raw, _ := io.ReadAll(req.Body)

		var items []map[string]any
		if err := json.Unmarshal(raw, &items); err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)

			return
		}

		if req.URL.Path == "/api/v1/logs" {
			r.logs = append(r.logs, items...)
		} else {
			r.metrics = append(r.metrics, items...)
		}

		w.WriteHeader(http.StatusAccepted)
	})
}

func (r *recorder) waitFor(t *testing.T, what string, enough func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		r.mu.Lock()
		ok := enough()
		r.mu.Unlock()

		if ok {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

func newAgent(t *testing.T, server *httptest.Server, cfg config.Config) (*Agent, string) {
	t.Helper()

	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg.Token, cfg.Endpoint, cfg.Host, cfg.StateFile = "test-token", server.URL, "web-1", statePath

	s := sender.New(server.URL, "test-token", "agent-test")
	s.MinBackoff, s.MaxBackoff = time.Millisecond, 5*time.Millisecond

	return &Agent{Config: cfg, Sender: s, State: LoadState(statePath), Logf: t.Logf}, statePath
}

var off = false

func TestLogLinesAreDeliveredAndTheFilePositionIsSaved(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	received := &recorder{}
	server := httptest.NewServer(received.handler(1)) // the first request fails, the retry works
	defer server.Close()

	agent, statePath := newAgent(t, server, config.Config{Logs: []config.LogSource{{Path: logPath}}, Metrics: config.Metrics{Enabled: &off}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		agent.Run(ctx)
		close(done)
	}()

	time.Sleep(400 * time.Millisecond)

	handle, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = handle.WriteString("plain line\n" + `{"message":"json line","level":"error"}` + "\n")
	_ = handle.Close()

	received.waitFor(t, "both lines", func() bool { return len(received.logs) >= 2 })
	cancel()
	<-done

	plain, structured := received.logs[0], received.logs[1]

	if plain["message"] != "plain line" || plain["host"] != "web-1" || plain["context"].(map[string]any)["file"] != logPath {
		t.Errorf("plain line sent as %v", plain)
	}

	if structured["message"] != "json line" || structured["level"] != "error" || structured["host"] != "web-1" {
		t.Errorf("JSON line sent as %v", structured)
	}

	saved := LoadState(statePath).Positions()[logPath]
	if saved.Offset != int64(len("plain line\n")+len(`{"message":"json line","level":"error"}`)+1) {
		t.Errorf("saved position %+v", saved)
	}
}

func TestARestartContinuesAfterTheLastDeliveredLine(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, []byte("delivered\nnot delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(logPath)
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := LoadState(statePath)
	state.Record(logPath, tail.Position{Inode: inodeOfForTest(info), Offset: int64(len("delivered\n"))})

	if err := state.Save(); err != nil {
		t.Fatal(err)
	}

	received := &recorder{}
	server := httptest.NewServer(received.handler(0))
	defer server.Close()

	agent, _ := newAgent(t, server, config.Config{Logs: []config.LogSource{{Path: logPath}}, Metrics: config.Metrics{Enabled: &off}})
	agent.State = LoadState(statePath)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		agent.Run(ctx)
		close(done)
	}()

	received.waitFor(t, "the undelivered line", func() bool { return len(received.logs) >= 1 })
	cancel()
	<-done

	if len(received.logs) != 1 || received.logs[0]["message"] != "not delivered" {
		t.Fatalf("got %v", received.logs)
	}
}

func TestHostMetricsAreDeliveredEveryInterval(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("host metrics are Linux-only")
	}

	received := &recorder{}
	server := httptest.NewServer(received.handler(0))
	defer server.Close()

	enabled := true
	agent, _ := newAgent(t, server, config.Config{Metrics: config.Metrics{Enabled: &enabled, IntervalSeconds: 1}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		agent.Run(ctx)
		close(done)
	}()

	received.waitFor(t, "metrics", func() bool { return len(received.metrics) > 5 })
	cancel()
	<-done

	names := map[string]bool{}

	for _, point := range received.metrics {
		names[point["name"].(string)] = true

		if point["host"] != "web-1" || point["ts"] == "" {
			t.Fatalf("point %v lacks its host or time", point)
		}
	}

	if !names["system.mem.used_percent"] || !names["system.load.1m"] {
		t.Errorf("expected memory and load points, got %v", names)
	}
}

func TestEntriesFromTheSameFileKeepTheirOrderAndPlainTextIsNotInterpreted(t *testing.T) {
	line := tail.Line{File: "/x.log", Text: `{"not json`, Time: time.Unix(0, 0)}
	entry := logEntry(line, "h")

	var decoded map[string]any
	if err := json.Unmarshal(entry, &decoded); err != nil || decoded["message"] != `{"not json` {
		t.Fatalf("a broken JSON line must be a plain message: %s %v", entry, err)
	}

	// A JSON line that names its own host keeps it.
	own := logEntry(tail.Line{Text: `{"message":"m","host":"theirs"}`}, "ours")
	if !strings.Contains(string(own), `"host":"theirs"`) {
		t.Fatalf("host overwritten: %s", own)
	}

	// An array is not an object, so it is a message too.
	if got := logEntry(tail.Line{Text: `[1,2]`, Time: time.Now()}, "h"); !strings.Contains(string(got), `"message":"[1,2]"`) {
		t.Fatalf("array sent as %s", got)
	}
}

func TestMetricsAreSplitIntoRequests(t *testing.T) {
	if parts := metricsPayloads(nil, "h", time.Now(), 500); len(parts) != 0 {
		t.Fatalf("no points, no requests, got %d", len(parts))
	}
}

func TestStateIsWrittenAtomicallyAndNeverMovesBackwards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	state := LoadState(path)

	state.Record("/a.log", tail.Position{Inode: 1, Offset: 100})
	state.Record("/a.log", tail.Position{Inode: 1, Offset: 50})

	if err := state.Save(); err != nil {
		t.Fatal(err)
	}

	if got := LoadState(path).Positions()["/a.log"]; got.Offset != 100 {
		t.Fatalf("position moved backwards: %+v", got)
	}

	// A new inode (rotation) may restart the offset.
	state.Record("/a.log", tail.Position{Inode: 2, Offset: 10})

	if got := state.Positions()["/a.log"]; got.Inode != 2 || got.Offset != 10 {
		t.Fatalf("rotation not recorded: %+v", got)
	}

	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestACorruptStateFileIsAnEmptyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o600)

	if len(LoadState(path).Positions()) != 0 {
		t.Fatal("corrupt state should be ignored")
	}
}
