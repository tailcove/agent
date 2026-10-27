package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func str(s string) *string { return &s }

func run(m Monitor) Result {
	m.Type = "http"
	if m.TimeoutMS == 0 {
		m.TimeoutMS = 3000
	}

	return Checker{Guard: Guard{AllowPrivate: true}}.Run(context.Background(), m)
}

func TestHTTPUpAndStatusMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			w.WriteHeader(500)

			return
		}

		_, _ = w.Write([]byte("hello"))
	}))
	defer server.Close()

	if r := run(Monitor{ID: 7, URL: server.URL + "/ok", ExpectedStatus: 200}); !r.OK || r.MonitorID != 7 || r.StatusCode != 200 {
		t.Errorf("ok check: %+v", r)
	}

	r := run(Monitor{URL: server.URL + "/broken", ExpectedStatus: 200})
	if r.OK || r.Error != "Expected HTTP 200, got 500" || r.StatusCode != 500 {
		t.Errorf("status mismatch: %+v", r)
	}
}

func TestHTTPKeyword(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("Welcome back")) }))
	defer server.Close()

	if r := run(Monitor{URL: server.URL, Keyword: str("Welcome")}); !r.OK {
		t.Errorf("keyword present: %+v", r)
	}

	if r := run(Monitor{URL: server.URL, Keyword: str("Goodbye")}); r.OK || !strings.HasPrefix(r.Error, "Keyword") {
		t.Errorf("keyword absent: %+v", r)
	}
}

func TestHTTPResponseTooLargeForKeyword(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(maxBody+10))
		_, _ = w.Write([]byte(strings.Repeat("a", 100)))
	}))
	defer server.Close()

	if r := run(Monitor{URL: server.URL, Keyword: str("a")}); r.OK || r.Error != "Response too large" {
		t.Errorf("too large: %+v", r)
	}
}

func TestHTTPSendsMethodHeadersAndBody(t *testing.T) {
	var got struct {
		method, key, body, contentType string
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buffer := make([]byte, 64)
		n, _ := r.Body.Read(buffer)
		got.method, got.key, got.body, got.contentType = r.Method, r.Header.Get("X-Api-Key"), string(buffer[:n]), r.Header.Get("Content-Type")
	}))
	defer server.Close()

	r := run(Monitor{URL: server.URL, Method: "put", Headers: map[string]string{"X-Api-Key": "k-1"}, Body: str(`{"a":1}`)})

	if !r.OK || got.method != "PUT" || got.key != "k-1" || got.body != `{"a":1}` || got.contentType != "text/plain" {
		t.Errorf("request: %+v result %+v", got, r)
	}
}

func TestHTTPNeverSetsForbiddenHeaders(t *testing.T) {
	var host, forwarded, proxy string

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		host, forwarded, proxy = r.Host, r.Header.Get("X-Forwarded-For"), r.Header.Get("Proxy-Foo")
	}))
	defer server.Close()

	run(Monitor{URL: server.URL, Headers: map[string]string{"Host": "evil.test", "X-Forwarded-For": "10.0.0.1", "Proxy-Foo": "x", "X-Ok\r\nInjected": "y"}})

	if strings.Contains(host, "evil") || forwarded != "" || proxy != "" {
		t.Errorf("forbidden headers were sent: host=%q xff=%q proxy=%q", host, forwarded, proxy)
	}
}

func TestHTTPRedirectKeepsHeadersAtTheSameOriginOnly(t *testing.T) {
	var otherAuth, otherBody string
	var otherHits atomic.Int32

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		buffer := make([]byte, 16)
		n, _ := r.Body.Read(buffer)
		otherAuth, otherBody = r.Header.Get("Authorization"), string(buffer[:n])
	}))
	defer other.Close()

	var sameAuth string

	origin := httptest.NewServer(nil)
	defer origin.Close()

	origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/landed", http.StatusTemporaryRedirect)
		case "/landed":
			sameAuth = r.Header.Get("Authorization")
		default:
			http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
		}
	})

	headers := map[string]string{"Authorization": "Bearer secret"}

	if r := run(Monitor{URL: origin.URL + "/same", Headers: headers}); !r.OK || sameAuth != "Bearer secret" {
		t.Errorf("same-origin redirect lost the header: %+v auth=%q", r, sameAuth)
	}

	r := run(Monitor{URL: origin.URL + "/away", Method: "POST", Headers: headers, Body: str("private")})

	if !r.OK || otherHits.Load() != 1 || otherAuth != "" || otherBody != "" {
		t.Errorf("cross-origin redirect leaked: %+v hits=%d auth=%q body=%q", r, otherHits.Load(), otherAuth, otherBody)
	}
}

func TestHTTPRedirectToAnInternalTargetIsRefused(t *testing.T) {
	var hits atomic.Int32

	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer internal.Close()

	// A guard that allows only what AllowPrivate=false allows, except the first hop (the test server itself is loopback).
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer first.Close()

	// With the default guard even the first hop is refused, and nothing is contacted.
	r := Checker{Guard: Guard{}}.Run(context.Background(), Monitor{Type: "http", URL: first.URL, TimeoutMS: 2000})

	if r.OK || r.Error != "URL not allowed" || hits.Load() != 0 {
		t.Errorf("internal target: %+v hits=%d", r, hits.Load())
	}
}

func TestHTTPRefusesWhatTheGuardRefuses(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1:1/", "http://169.254.169.254/", "http://localhost/", "file:///etc/passwd", "http://2130706433/"} {
		r := Checker{Guard: Guard{}}.Run(context.Background(), Monitor{Type: "http", URL: target, TimeoutMS: 2000})

		if r.OK || r.Error != "URL not allowed" {
			t.Errorf("%s: %+v", target, r)
		}
	}
}

func TestHTTPTooManyRedirectsAndTimeout(t *testing.T) {
	loop := httptest.NewServer(nil)
	defer loop.Close()

	loop.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/again", http.StatusFound) })

	if r := run(Monitor{URL: loop.URL}); r.OK || r.Error != "Too many redirects" {
		t.Errorf("loop: %+v", r)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(3 * time.Second) }))
	defer slow.Close()

	started := time.Now()

	if r := run(Monitor{URL: slow.URL, TimeoutMS: 1000}); r.OK || r.Error != "Timed out" || time.Since(started) > 2500*time.Millisecond {
		t.Errorf("timeout: %+v after %s", r, time.Since(started))
	}
}

func TestHTTPRecordsTimings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write([]byte("x"))
	}))
	defer server.Close()

	r := run(Monitor{URL: server.URL})

	if !r.OK || r.TTFBMS < 20 || r.ResponseMS < r.TTFBMS {
		t.Errorf("timings: %+v", r)
	}
}

func TestTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback socket")
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	checker := Checker{Guard: Guard{AllowPrivate: true}}

	if r := checker.Run(context.Background(), Monitor{Type: "tcp", Host: "127.0.0.1", Port: port, TimeoutMS: 2000}); !r.OK {
		t.Errorf("open port: %+v", r)
	}

	_ = listener.Close()

	if r := checker.Run(context.Background(), Monitor{Type: "tcp", Host: "127.0.0.1", Port: port, TimeoutMS: 2000}); r.OK || r.Error != "Connection refused" {
		t.Errorf("closed port: %+v", r)
	}

	for _, host := range []string{"127.0.0.1", "10.0.0.1", "localhost", "169.254.169.254", "db"} {
		if r := (Checker{Guard: Guard{}}).Run(context.Background(), Monitor{Type: "tcp", Host: host, Port: 80, TimeoutMS: 2000}); r.OK || r.Error != "Host not allowed" {
			t.Errorf("%s: %+v", host, r)
		}
	}

	if r := checker.Run(context.Background(), Monitor{Type: "tcp", Host: "127.0.0.1", Port: 0, TimeoutMS: 2000}); r.OK {
		t.Errorf("port 0: %+v", r)
	}
}

func TestUnknownTypeAndOddMonitorsNeverPanic(t *testing.T) {
	checker := Checker{Guard: Guard{}}

	for _, m := range []Monitor{{Type: "icmp"}, {Type: "http"}, {Type: "http", URL: "::::"}, {Type: "http", URL: "http://example.com", Method: "TRACE"}, {Type: "tcp"}, {Type: "http", URL: "http://example.com", Headers: map[string]string{"a": "b", "c": "d", "e": "f", "g": "h", "i": "j", "k": "l", "m": "n", "o": "p", "q": "r", "s": "t", "u": "v"}}} {
		if r := checker.Run(context.Background(), m); r.OK {
			t.Errorf("%+v was up", m)
		}
	}
}
