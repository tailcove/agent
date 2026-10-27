// Package sender posts batches to the Tailcove ingest API and retries the
// failures that are worth retrying.
package sender

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Outcome says what became of a batch.
type Outcome int

const (
	// Delivered: the API accepted the batch.
	Delivered Outcome = iota
	// Dropped: the API refused the batch for what it holds (too large, not
	// valid); sending it again would fail the same way.
	Dropped
	// Cancelled: the context ended before the batch was accepted.
	Cancelled
)

// Sender posts JSON batches.
type Sender struct {
	Endpoint  string
	Token     string
	UserAgent string
	Client    *http.Client
	// Backoff bounds the wait between attempts; zero values use 1s and 60s.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// Logf receives one line per failed attempt; nil discards them.
	Logf func(format string, args ...any)
	// Sleep waits between attempts and returns false if the context ended.
	// Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) bool
}

// New returns a Sender with a client that gives up on a stuck request.
func New(endpoint, token, userAgent string) *Sender {
	return &Sender{
		Endpoint:  endpoint,
		Token:     token,
		UserAgent: userAgent,
		Client:    &http.Client{Timeout: 30 * time.Second},
	}
}

// WithToken is a sender like this one that posts with another source's token:
// the same endpoint, client and backoff, and its own retries.
func (s *Sender) WithToken(token string) *Sender {
	other := *s
	other.Token = token

	return &other
}

// Post sends body to path (for example /api/v1/logs) until the API accepts it,
// refuses it for good, or ctx ends. Network errors, 429, 503 and other 5xx are
// retried with exponential backoff and jitter (a Retry-After header is
// honoured); 401, 403 and 404 are retried slowly, because a token or an
// endpoint can be fixed while the agent runs; 400, 413 and 422 drop the batch.
func (s *Sender) Post(ctx context.Context, path string, body []byte) Outcome {
	minBackoff, maxBackoff := s.MinBackoff, s.MaxBackoff

	if minBackoff <= 0 {
		minBackoff = time.Second
	}

	if maxBackoff <= 0 {
		maxBackoff = time.Minute
	}

	wait := minBackoff

	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return Cancelled
		}

		status, retryAfter, err := s.attempt(ctx, path, body)

		switch {
		case err == nil && status >= 200 && status < 300:
			return Delivered
		case err == nil && (status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge || status == http.StatusUnprocessableEntity):
			s.logf("dropping a batch the API refused (HTTP %d)", status)

			return Dropped
		case ctx.Err() != nil:
			return Cancelled
		}

		delay := jitter(wait)

		switch {
		case err != nil:
			s.logf("send to %s failed (attempt %d): %v", path, attempt, err)
		case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
			s.logf("the API answered HTTP %d for %s: check the token and the endpoint; trying again in a while", status, path)

			delay = max(delay, maxBackoff/2)
		default:
			s.logf("send to %s failed (attempt %d): HTTP %d", path, attempt, status)
		}

		if retryAfter > 0 {
			delay = max(delay, retryAfter)
		}

		if !s.sleep(ctx, delay) {
			return Cancelled
		}

		wait = min(wait*2, maxBackoff)
	}
}

func (s *Sender) attempt(ctx context.Context, path string, body []byte) (int, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}

	request.Header.Set("Authorization", "Bearer "+s.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", s.UserAgent)

	response, err := s.Client.Do(request)
	if err != nil {
		return 0, 0, errors.New(redact(err, s.Token))
	}

	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))

	return response.StatusCode, parseRetryAfter(response.Header.Get("Retry-After")), nil
}

func (s *Sender) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Sender) sleep(ctx context.Context, d time.Duration) bool {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// parseRetryAfter reads the delta-seconds form; anything else is ignored.
func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return 0
	}

	return min(time.Duration(seconds), 3600) * time.Second
}

// jitter spreads d over 50%..100% so agents that failed together do not retry together.
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// redact keeps the token out of an error text, whatever put it there.
func redact(err error, token string) string {
	text := err.Error()

	if token != "" {
		text = string(bytes.ReplaceAll([]byte(text), []byte(token), []byte("[token]")))
	}

	return fmt.Sprint(text)
}
