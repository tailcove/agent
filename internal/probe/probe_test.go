package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeAPI struct {
	mu       sync.Mutex
	monitors []Monitor
	status   int
	posted   []Result
	auth     []string
	posts    int
	failPost int
}

func (f *fakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		f.auth = append(f.auth, r.Header.Get("Authorization"))

		if f.status != 0 {
			w.WriteHeader(f.status)

			return
		}

		switch r.URL.Path {
		case "/api/v1/probe/monitors":
			_ = json.NewEncoder(w).Encode(map[string]any{"monitors": f.monitors, "poll_seconds": 7})
		case "/api/v1/probe/results":
			f.posts++

			if f.posts <= f.failPost {
				w.WriteHeader(http.StatusServiceUnavailable)

				return
			}

			var body struct {
				Results []Result `json:"results"`
			}

			_ = json.NewDecoder(r.Body).Decode(&body)
			f.posted = append(f.posted, body.Results...)
			_ = json.NewEncoder(w).Encode(map[string]int{"accepted": len(body.Results)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newProbe(api *httptest.Server) *Probe {
	p := New(api.URL, "tcp_token", "test")
	p.Checker = Checker{Guard: Guard{AllowPrivate: true}}
	p.Sleep = func(context.Context, time.Duration) bool { return true }

	return p
}

func TestCyclePullsRunsAndPosts(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer target.Close()

	api := &fakeAPI{monitors: []Monitor{
		{ID: 1, Type: "http", URL: target.URL, ExpectedStatus: 200, TimeoutMS: 2000},
		{ID: 2, Type: "http", URL: target.URL, ExpectedStatus: 404, TimeoutMS: 2000},
	}}
	server := httptest.NewServer(api.handler())
	defer server.Close()

	wait := newProbe(server).Cycle(context.Background())

	if wait != 7*time.Second {
		t.Errorf("wait = %s, want the server's 7s", wait)
	}

	byID := map[int64]Result{}
	for _, r := range api.posted {
		byID[r.MonitorID] = r
	}

	if len(byID) != 2 || !byID[1].OK || byID[2].OK {
		t.Errorf("posted %+v", api.posted)
	}

	for _, header := range api.auth {
		if header != "Bearer tcp_token" {
			t.Errorf("Authorization = %q", header)
		}
	}
}

func TestCycleWithNothingDueDoesNotPost(t *testing.T) {
	api := &fakeAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()

	newProbe(server).Cycle(context.Background())

	if api.posts != 0 {
		t.Errorf("posted %d times with nothing due", api.posts)
	}
}

func TestCycleBacksOffOnARefusedTokenAndOnErrors(t *testing.T) {
	for status, want := range map[int]time.Duration{401: authBackoff, 500: errorBackoff, 429: errorBackoff} {
		api := &fakeAPI{status: status}
		server := httptest.NewServer(api.handler())

		var logged string

		p := newProbe(server)
		p.Logf = func(format string, _ ...any) { logged = format }

		if got := p.Cycle(context.Background()); got != want || logged == "" {
			t.Errorf("status %d: wait %s (want %s), logged %q", status, got, want, logged)
		}

		server.Close()
	}
}

func TestPostRetriesATransientFailureOnce(t *testing.T) {
	api := &fakeAPI{failPost: 1, monitors: []Monitor{{ID: 1, Type: "tcp", Host: "127.0.0.1", Port: 1, TimeoutMS: 1000}}}
	server := httptest.NewServer(api.handler())
	defer server.Close()

	newProbe(server).Cycle(context.Background())

	if api.posts != 2 || len(api.posted) != 1 {
		t.Errorf("posts=%d posted=%d", api.posts, len(api.posted))
	}
}

func TestResultsAreBatched(t *testing.T) {
	var monitors []Monitor
	for i := 1; i <= 230; i++ {
		monitors = append(monitors, Monitor{ID: int64(i), Type: "tcp", Host: "127.0.0.1", Port: 1, TimeoutMS: 1000})
	}

	api := &fakeAPI{monitors: monitors}
	server := httptest.NewServer(api.handler())
	defer server.Close()

	p := newProbe(server)
	p.Concurrency = 100
	p.Cycle(context.Background())

	if api.posts != 3 || len(api.posted) != 230 {
		t.Errorf("posts=%d posted=%d", api.posts, len(api.posted))
	}
}

func TestRunStopsWhenTheContextEnds(t *testing.T) {
	api := &fakeAPI{}
	server := httptest.NewServer(api.handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := newProbe(server)
	p.Sleep = func(context.Context, time.Duration) bool { cancel(); return false }

	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}
