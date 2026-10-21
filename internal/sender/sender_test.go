package sender

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fast(s *Sender) *Sender {
	s.MinBackoff = time.Millisecond
	s.MaxBackoff = 4 * time.Millisecond
	s.Sleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }

	return s
}

func TestDeliversWithTheTokenAndTheBody(t *testing.T) {
	var gotAuth, gotType, gotBody, gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType, gotPath = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)

		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	outcome := fast(New(server.URL, "secret-token", "agent/test")).Post(context.Background(), "/api/v1/logs", []byte(`[{"message":"hi"}]`))

	if outcome != Delivered || gotAuth != "Bearer secret-token" || gotType != "application/json" || gotPath != "/api/v1/logs" || gotBody != `[{"message":"hi"}]` {
		t.Fatalf("outcome %v auth %q type %q path %q body %q", outcome, gotAuth, gotType, gotPath, gotBody)
	}
}

func TestRetriesServerErrorsAndRateLimitsUntilAccepted(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer server.Close()

	var waits []time.Duration

	s := fast(New(server.URL, "t", "a"))
	s.Sleep = func(ctx context.Context, d time.Duration) bool {
		waits = append(waits, d)

		return true
	}

	if outcome := s.Post(context.Background(), "/api/v1/logs", []byte("[]")); outcome != Delivered || calls.Load() != 3 {
		t.Fatalf("outcome %v after %d calls", outcome, calls.Load())
	}

	if len(waits) != 2 || waits[1] < time.Second {
		t.Fatalf("a Retry-After of 1s was not honoured: %v", waits)
	}
}

func TestDropsABatchTheAPIRefusesForWhatItHolds(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
		var calls atomic.Int32

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
		}))

		if outcome := fast(New(server.URL, "t", "a")).Post(context.Background(), "/api/v1/logs", []byte("[]")); outcome != Dropped || calls.Load() != 1 {
			t.Errorf("HTTP %d: outcome %v after %d calls", status, outcome, calls.Load())
		}

		server.Close()
	}
}

func TestAWrongTokenIsRetriedNotDropped(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	if outcome := fast(New(server.URL, "t", "a")).Post(context.Background(), "/api/v1/logs", []byte("[]")); outcome != Delivered || calls.Load() != 3 {
		t.Fatalf("outcome %v after %d calls", outcome, calls.Load())
	}
}

func TestStopsWhenTheContextEnds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())

	s := fast(New(server.URL, "t", "a"))
	s.Sleep = func(ctx context.Context, d time.Duration) bool {
		cancel()

		return false
	}

	if outcome := s.Post(ctx, "/api/v1/logs", []byte("[]")); outcome != Cancelled {
		t.Fatalf("outcome %v", outcome)
	}
}

func TestTheTokenNeverAppearsInALoggedError(t *testing.T) {
	var logged []string

	s := fast(New("http://127.0.0.1:1/secret-token", "secret-token", "a"))
	s.Logf = func(format string, args ...any) {
		logged = append(logged, strings.TrimSpace(format+" "+strings.Join(stringify(args), " ")))
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.Sleep = func(ctx context.Context, d time.Duration) bool {
		cancel()

		return false
	}

	s.Post(ctx, "/x", []byte("[]"))

	if len(logged) == 0 {
		t.Fatal("nothing was logged")
	}

	for _, line := range logged {
		if strings.Contains(line, "secret-token") {
			t.Fatalf("token leaked: %s", line)
		}
	}
}

func stringify(args []any) []string {
	out := make([]string, len(args))

	for i, a := range args {
		switch v := a.(type) {
		case error:
			out[i] = v.Error()
		case string:
			out[i] = v
		default:
			out[i] = ""
		}
	}

	return out
}

func TestJitterStaysWithinHalfToFull(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := jitter(time.Second)
		if d < 500*time.Millisecond || d > time.Second {
			t.Fatalf("jitter(1s) = %v", d)
		}
	}
}
