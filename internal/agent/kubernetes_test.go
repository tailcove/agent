package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tailcove/agent/internal/config"
	"github.com/tailcove/agent/internal/sender"
)

// tokenRecorder keeps what each token delivered; tokens in refused get a 401.
type tokenRecorder struct {
	mu      sync.Mutex
	logs    map[string][]map[string]any
	refused map[string]bool
}

func newTokenRecorder(refused ...string) *tokenRecorder {
	r := &tokenRecorder{logs: map[string][]map[string]any{}, refused: map[string]bool{}}
	for _, token := range refused {
		r.refused[token] = true
	}

	return r
}

func (r *tokenRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if r.refused[token] {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	raw, _ := io.ReadAll(req.Body)

	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)

		return
	}

	if req.URL.Path == "/api/v1/logs" {
		r.logs[token] = append(r.logs[token], items...)
	}

	w.WriteHeader(http.StatusAccepted)
}

func (r *tokenRecorder) count(token string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.logs[token])
}

func (r *tokenRecorder) entries(token string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]map[string]any(nil), r.logs[token]...)
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()

	deadline := time.Now().Add(6 * time.Second)

	for time.Now().Before(deadline) {
		if ok() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

func podLog(t *testing.T, podsDir, ns, pod, container, name string) string {
	t.Helper()

	dir := filepath.Join(podsDir, ns+"_"+pod+"_0a1b2c3d-0000-0000-0000-000000000001", container)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()

	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = handle.WriteString(text)
	_ = handle.Close()
}

func cri(stream, text string) string {
	return "2026-10-27T10:00:00.123456789Z " + stream + " F " + text + "\n"
}

type kubeRun struct {
	agent  *Agent
	state  string
	cancel context.CancelFunc
	done   chan struct{}
}

func (k *kubeRun) stop() {
	k.cancel()
	<-k.done
}

func startKube(t *testing.T, server *httptest.Server, podsDir string, kcfg config.Kubernetes, statePath string) *kubeRun {
	t.Helper()

	if statePath == "" {
		statePath = filepath.Join(t.TempDir(), "state.json")
	}

	api := false
	kcfg.Enabled, kcfg.PodsDir, kcfg.NodeName, kcfg.API = true, podsDir, "node-1", &api
	kcfg.Host = "pod"
	kcfg.Labels = config.DefaultKubernetesLabels
	kcfg.Metrics = config.KubernetesMetrics{Enabled: &off, IntervalSeconds: 30}

	s := sender.New(server.URL, "main-token", "agent-test")
	s.MinBackoff, s.MaxBackoff = time.Millisecond, 5*time.Millisecond

	cfg := config.Config{Token: "main-token", Endpoint: server.URL, Host: "node-1", StateFile: statePath, Metrics: config.Metrics{Enabled: &off}, Kubernetes: kcfg}
	agent := &Agent{Config: cfg, Sender: s, State: LoadState(statePath), Logf: t.Logf, Kube: NewKubernetes(kcfg, t.Logf)}

	ctx, cancel := context.WithCancel(context.Background())
	run := &kubeRun{agent: agent, state: statePath, cancel: cancel, done: make(chan struct{})}

	go func() {
		agent.Run(ctx)
		close(run.done)
	}()

	return run
}

func TestPodLogsCarryTheirPodAndStackTracesAreJoined(t *testing.T) {
	podsDir := t.TempDir()
	logFile := podLog(t, podsDir, "shop", "web-7d9f-abcde", "app", "0.log")

	recorded := newTokenRecorder()
	server := httptest.NewServer(recorded)
	defer server.Close()

	run := startKube(t, server, podsDir, config.Kubernetes{FromBeginning: true}, "")
	defer run.stop()

	time.Sleep(300 * time.Millisecond)
	appendTo(t, logFile,
		cri("stdout", "starting up")+
			cri("stderr", `{"message":"payment failed","level":"error"}`)+
			cri("stderr", "Exception: boom")+
			cri("stderr", "    at one()")+
			cri("stderr", "    at two()")+
			cri("stdout", "2026-10-27 10:00:01 INFO next event"))

	waitUntil(t, "four entries", func() bool { return recorded.count("main-token") >= 4 })

	entries := recorded.entries("main-token")

	if entries[0]["message"] != "starting up" || entries[0]["host"] != "web-7d9f-abcde" {
		t.Errorf("plain line: %v", entries[0])
	}

	kubernetes := entries[0]["context"].(map[string]any)["kubernetes"].(map[string]any)
	if kubernetes["namespace"] != "shop" || kubernetes["container"] != "app" || kubernetes["pod"] != "web-7d9f-abcde" {
		t.Errorf("context: %v", kubernetes)
	}

	if entries[1]["message"] != "payment failed" || entries[1]["level"] != "error" || entries[1]["host"] != "web-7d9f-abcde" || entries[1]["ts"] == nil {
		t.Errorf("JSON line passed through: %v", entries[1])
	}

	trace, _ := entries[2]["message"].(string)
	if !strings.Contains(trace, "Exception: boom") || !strings.Contains(trace, "at two()") {
		t.Errorf("stack trace not joined: %v", entries[2])
	}
}

func TestExcludedNamespacesAreNotRead(t *testing.T) {
	podsDir := t.TempDir()
	kept := podLog(t, podsDir, "shop", "web-1", "app", "0.log")
	skipped := podLog(t, podsDir, "kube-system", "dns-1", "coredns", "0.log")

	recorded := newTokenRecorder()
	server := httptest.NewServer(recorded)
	defer server.Close()

	run := startKube(t, server, podsDir, config.Kubernetes{FromBeginning: true, ExcludeNamespaces: []string{"kube-*"}}, "")
	defer run.stop()

	time.Sleep(300 * time.Millisecond)
	appendTo(t, skipped, cri("stdout", "hidden"))
	appendTo(t, kept, cri("stdout", "shown"))

	waitUntil(t, "the shown line", func() bool { return recorded.count("main-token") >= 1 })
	time.Sleep(200 * time.Millisecond)

	entries := recorded.entries("main-token")
	if len(entries) != 1 || entries[0]["message"] != "shown" {
		t.Errorf("unexpected entries: %v", entries)
	}
}

func TestARouteSendsAPodToAnotherSourceAndABadRouteStallsNothing(t *testing.T) {
	podsDir := t.TempDir()
	billing := podLog(t, podsDir, "billing", "api-1", "app", "0.log")
	broken := podLog(t, podsDir, "broken", "api-2", "app", "0.log")
	shop := podLog(t, podsDir, "shop", "web-1", "app", "0.log")

	recorded := newTokenRecorder("refused-token")
	server := httptest.NewServer(recorded)
	defer server.Close()

	routes := []config.KubernetesRoute{
		{Namespace: "billing", Token: "billing-token"},
		{Namespace: "broken", Token: "refused-token"},
	}

	run := startKube(t, server, podsDir, config.Kubernetes{FromBeginning: true, Routes: routes}, "")
	defer run.stop()

	time.Sleep(300 * time.Millisecond)

	var lines strings.Builder
	for i := 0; i < 3000; i++ {
		lines.WriteString(cri("stdout", "refused line"))
	}

	appendTo(t, broken, lines.String())
	appendTo(t, billing, cri("stdout", "invoice paid"))
	appendTo(t, shop, cri("stdout", "order placed"))

	waitUntil(t, "the routed and the main line", func() bool {
		return recorded.count("billing-token") >= 1 && recorded.count("main-token") >= 1
	})

	if got := recorded.entries("billing-token")[0]["message"]; got != "invoice paid" {
		t.Errorf("routed line: %v", got)
	}

	if got := recorded.entries("main-token")[0]["message"]; got != "order placed" {
		t.Errorf("main line: %v", got)
	}

	if recorded.count("refused-token") != 0 {
		t.Error("a refused token delivered something")
	}
}

func TestAPodThatRestartsContinuesInItsNewFileAndARestartOfTheAgentDoesNotRepeat(t *testing.T) {
	podsDir := t.TempDir()
	first := podLog(t, podsDir, "shop", "web-1", "app", "0.log")

	recorded := newTokenRecorder()
	server := httptest.NewServer(recorded)
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")

	run := startKube(t, server, podsDir, config.Kubernetes{FromBeginning: true}, statePath)

	time.Sleep(300 * time.Millisecond)
	appendTo(t, first, cri("stdout", "before crash"))

	waitUntil(t, "the first line", func() bool { return recorded.count("main-token") >= 1 })

	// The container restarts: the runtime starts 1.log next to 0.log.
	second := podLog(t, podsDir, "shop", "web-1", "app", "1.log")
	appendTo(t, second, cri("stdout", "after restart"))

	waitUntil(t, "the line of the new file", func() bool { return recorded.count("main-token") >= 2 })

	// The position is recorded once the batch is accepted; stopping before that would (rightly) send the line again.
	waitUntil(t, "the position of the new file", func() bool { return run.agent.State.Positions()[second].Offset > 0 })
	run.stop()

	// The agent restarts: nothing delivered is sent again, what came later is.
	appendTo(t, second, cri("stdout", "while the agent was down"))

	again := startKube(t, server, podsDir, config.Kubernetes{}, statePath)
	defer again.stop()

	waitUntil(t, "the line written while down", func() bool { return recorded.count("main-token") >= 3 })
	time.Sleep(300 * time.Millisecond)

	var messages []string
	for _, entry := range recorded.entries("main-token") {
		messages = append(messages, entry["message"].(string))
	}

	want := []string{"before crash", "after restart", "while the agent was down"}
	if strings.Join(messages, "|") != strings.Join(want, "|") {
		t.Errorf("messages %v, want %v", messages, want)
	}
}

func TestARotatedLogIsFollowedInItsNewFile(t *testing.T) {
	podsDir := t.TempDir()
	current := podLog(t, podsDir, "shop", "web-1", "app", "0.log")

	recorded := newTokenRecorder()
	server := httptest.NewServer(recorded)
	defer server.Close()

	run := startKube(t, server, podsDir, config.Kubernetes{FromBeginning: true}, "")
	defer run.stop()

	time.Sleep(300 * time.Millisecond)
	appendTo(t, current, cri("stdout", "one"))
	waitUntil(t, "line one", func() bool { return recorded.count("main-token") >= 1 })

	// The kubelet renames the file and starts a new one with the same name.
	if err := os.Rename(current, current+".20261027-100000"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(current, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	appendTo(t, current, cri("stdout", "two"))
	waitUntil(t, "line two", func() bool { return recorded.count("main-token") >= 2 })

	entries := recorded.entries("main-token")
	if len(entries) != 2 || entries[1]["message"] != "two" {
		t.Errorf("unexpected entries: %v", entries)
	}
}
