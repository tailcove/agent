package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxRedirects  = 5
	maxBody       = 1 << 20
	maxTimeout    = 30 * time.Second
	minTimeout    = time.Second
	maxHeaders    = 10
	maxBodyToSend = 4096
)

// Monitor is one check Tailcove hands out. Fields a type does not use are zero.
type Monitor struct {
	ID              int64             `json:"id"`
	Type            string            `json:"type"`
	TimeoutMS       int               `json:"timeout_ms"`
	IntervalSeconds int               `json:"interval_seconds"`
	URL             string            `json:"url"`
	Method          string            `json:"method"`
	ExpectedStatus  int               `json:"expected_status"`
	Keyword         *string           `json:"keyword"`
	Headers         map[string]string `json:"headers"`
	Body            *string           `json:"body"`
	Host            string            `json:"host"`
	Port            int               `json:"port"`
}

// Result is what a check found, in the shape the API takes.
type Result struct {
	MonitorID  int64  `json:"monitor_id"`
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	ResponseMS int    `json:"response_ms"`
	Error      string `json:"error"`
	DNSMS      int    `json:"dns_ms"`
	ConnectMS  int    `json:"connect_ms"`
	TLSMS      int    `json:"tls_ms"`
	TTFBMS     int    `json:"ttfb_ms"`
}

// Checker runs checks. Every connection it makes goes through Guard.
type Checker struct {
	Guard Guard
	// Resolver looks names up; nil uses the system resolver.
	Resolver *net.Resolver
}

// Run performs one check. It never panics on what a monitor contains and it
// always returns a result: a check that cannot run is a failed check.
func (c Checker) Run(ctx context.Context, m Monitor) Result {
	timeout := time.Duration(m.TimeoutMS) * time.Millisecond
	if timeout < minTimeout {
		timeout = minTimeout
	}

	if timeout > maxTimeout {
		timeout = maxTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	var result Result

	switch m.Type {
	case "http":
		result = c.http(ctx, m)
	case "tcp":
		result = c.tcp(ctx, m)
	default:
		result = Result{Error: "Unsupported check"}
	}

	result.MonitorID = m.ID
	result.ResponseMS = int(time.Since(started).Milliseconds())

	return result
}

func (c Checker) dialer() *net.Dialer {
	return &net.Dialer{Timeout: maxTimeout, Control: c.Guard.Control, Resolver: c.Resolver}
}

func (c Checker) tcp(ctx context.Context, m Monitor) Result {
	if m.Port < 1 || m.Port > 65535 {
		return Result{Error: "URL not allowed"}
	}

	if err := c.Guard.CheckHost(m.Host); err != nil {
		return Result{Error: "Host not allowed"}
	}

	conn, err := c.dialer().DialContext(ctx, "tcp", net.JoinHostPort(m.Host, strconv.Itoa(m.Port)))
	if err != nil {
		return Result{Error: dialError(err)}
	}

	_ = conn.Close()

	return Result{OK: true}
}

func dialError(err error) string {
	switch {
	case errors.Is(err, ErrNotAllowed):
		return "Host not allowed"
	case errors.Is(err, context.DeadlineExceeded):
		return "Timed out"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "Could not resolve host"
	}

	if strings.Contains(err.Error(), "refused") {
		return "Connection refused"
	}

	if strings.Contains(err.Error(), "not allowed") {
		return "Host not allowed"
	}

	return "Connection failed"
}

type timings struct {
	dnsStart, connectStart, tlsStart, wroteRequest time.Time
	dns, connect, tls, ttfb                        time.Duration
}

func (t *timings) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart:     func(httptrace.DNSStartInfo) { t.dnsStart = time.Now() },
		DNSDone:      func(httptrace.DNSDoneInfo) { t.dns = time.Since(t.dnsStart) },
		ConnectStart: func(_, _ string) { t.connectStart = time.Now() },
		ConnectDone:  func(_, _ string, _ error) { t.connect = time.Since(t.connectStart) },
		TLSHandshakeStart: func() {
			t.tlsStart = time.Now()
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) { t.tls = time.Since(t.tlsStart) },
		WroteRequest:     func(httptrace.WroteRequestInfo) { t.wroteRequest = time.Now() },
		GotFirstResponseByte: func() {
			if !t.wroteRequest.IsZero() {
				t.ttfb = time.Since(t.wroteRequest)
			}
		},
	}
}

func (c Checker) http(ctx context.Context, m Monitor) Result {
	method := strings.ToUpper(m.Method)
	if method == "" {
		method = http.MethodGet
	}

	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		return Result{Error: "URL not allowed"}
	}

	if len(m.Headers) > maxHeaders {
		return Result{Error: "URL not allowed"}
	}

	target, err := c.Guard.CheckURL(m.URL)
	if err != nil {
		return Result{Error: "URL not allowed"}
	}

	origin := originOf(target)
	originalMethod := method
	current := target
	var phases timings

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext:           c.dialer().DialContext,
			TLSHandshakeTimeout:   maxTimeout,
			ResponseHeaderTimeout: maxTimeout,
			DisableCompression:    true,
			DisableKeepAlives:     true,
			Proxy:                 nil,
		},
	}

	for hop := 0; ; hop++ {
		sameOrigin := originOf(current) == origin
		var body io.Reader

		if sameOrigin && method == originalMethod && m.Body != nil && allowsBody(method) && len(*m.Body) <= maxBodyToSend {
			body = strings.NewReader(*m.Body)
		}

		request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, phases.trace()), method, current.String(), body)
		if err != nil {
			return Result{Error: "URL not allowed"}
		}

		// What the customer wrote goes only to the origin it was written for.
		if sameOrigin {
			for name, value := range m.Headers {
				if strings.ContainsAny(name+value, "\r\n\x00") || forbiddenHeader(name) {
					continue
				}

				request.Header.Set(name, value)
			}

			if body != nil && request.Header.Get("Content-Type") == "" {
				request.Header.Set("Content-Type", "text/plain")
			}
		}

		request.Header.Set("User-Agent", "tailcove-probe")

		response, err := client.Do(request)
		if err != nil {
			return failure(err, phases)
		}

		location := response.Header.Get("Location")

		if isRedirect(response.StatusCode) && location != "" {
			_ = response.Body.Close()

			if hop == maxRedirects {
				return Result{Error: "Too many redirects"}
			}

			next, err := current.Parse(location)
			if err != nil {
				return Result{Error: "Invalid redirect location"}
			}

			if _, err := c.Guard.CheckURL(next.String()); err != nil {
				return Result{Error: "URL not allowed"}
			}

			current = next

			if response.StatusCode == 303 || response.StatusCode <= 302 {
				if method != "GET" && method != "HEAD" && method != "OPTIONS" {
					method = "GET"
				}
			}

			phases = timings{}

			continue
		}

		return c.judge(m, method, response, phases)
	}
}

func (c Checker) judge(m Monitor, method string, response *http.Response, phases timings) Result {
	defer response.Body.Close()

	result := Result{
		StatusCode: response.StatusCode,
		DNSMS:      ms(phases.dns), ConnectMS: ms(phases.connect), TLSMS: ms(phases.tls), TTFBMS: ms(phases.ttfb),
	}

	expected := m.ExpectedStatus
	if expected == 0 {
		expected = 200
	}

	if response.StatusCode != expected {
		result.Error = fmt.Sprintf("Expected HTTP %d, got %d", expected, response.StatusCode)

		return result
	}

	if m.Keyword != nil && *m.Keyword != "" && method != "HEAD" {
		if response.ContentLength > maxBody {
			result.Error = "Response too large"

			return result
		}

		body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
		if err != nil {
			result.Error = "Connection failed"

			return result
		}

		if len(body) > maxBody {
			result.Error = "Response too large"

			return result
		}

		if !strings.Contains(string(body), *m.Keyword) {
			result.Error = fmt.Sprintf("Keyword %q not found in response", *m.Keyword)

			return result
		}
	}

	result.OK = true

	return result
}

func failure(err error, phases timings) Result {
	result := Result{DNSMS: ms(phases.dns), ConnectMS: ms(phases.connect), TLSMS: ms(phases.tls)}

	switch {
	case errors.Is(err, ErrNotAllowed), strings.Contains(err.Error(), ErrNotAllowed.Error()):
		result.Error = "URL not allowed"
	case errors.Is(err, context.DeadlineExceeded):
		result.Error = "Timed out"
	default:
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			result.Error = "Could not resolve host"
		} else if strings.Contains(err.Error(), "refused") {
			result.Error = "Connection refused"
		} else {
			result.Error = "Connection failed"
		}
	}

	return result
}

func originOf(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}

	return strings.ToLower(u.Scheme + "://" + u.Hostname() + ":" + port)
}

func allowsBody(method string) bool {
	return method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
}

func isRedirect(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	}

	return false
}

// forbiddenHeader mirrors the server's list: headers that describe the
// connection or the framing, or the host, are never set by a monitor.
func forbiddenHeader(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-length", "transfer-encoding", "connection", "upgrade", "te", "trailer", "keep-alive",
		"expect", "proxy-authorization", "proxy-connection", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-real-ip":
		return true
	}

	return strings.HasPrefix(strings.ToLower(name), "proxy-")
}

func ms(d time.Duration) int {
	if d < 0 {
		return 0
	}

	return int(d.Milliseconds())
}
