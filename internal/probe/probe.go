package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	pollMin       = 2 * time.Second
	pollMax       = time.Minute
	pollDefault   = 5 * time.Second
	errorBackoff  = 10 * time.Second
	authBackoff   = time.Minute
	maxListBytes  = 4 << 20
	resultBatch   = 100
	postAttempts  = 3
	defaultFanOut = 50
)

// Probe pulls the checks due for its region, runs them, and posts the results.
// It is report-only: it holds one token, scoped by the server to the probe
// endpoints, and no credential of any other kind.
type Probe struct {
	Endpoint  string
	Token     string
	Version   string
	UserAgent string
	Client    *http.Client
	Checker   Checker
	// Concurrency bounds the checks run at once; zero uses 50.
	Concurrency int
	// Logf receives one line per problem; nil discards them.
	Logf func(format string, args ...any)
	// Sleep waits and returns false if the context ended. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) bool
}

// New returns a Probe with a client that gives up on a stuck request.
func New(endpoint, token, version string) *Probe {
	return &Probe{
		Endpoint:  endpoint,
		Token:     token,
		Version:   version,
		UserAgent: "tailcove-agent/" + version,
		Client:    &http.Client{Timeout: 30 * time.Second},
	}
}

type listResponse struct {
	Monitors    []Monitor `json:"monitors"`
	PollSeconds int       `json:"poll_seconds"`
}

// Run cycles until ctx ends.
func (p *Probe) Run(ctx context.Context) {
	for ctx.Err() == nil {
		wait := p.Cycle(ctx)

		if !p.sleep(ctx, wait) {
			return
		}
	}
}

// Cycle is one round: ask for what is due, run it, post the results. It
// returns how long to wait before the next round.
func (p *Probe) Cycle(ctx context.Context) time.Duration {
	list, status, err := p.fetch(ctx)

	switch {
	case err != nil && status == http.StatusUnauthorized:
		p.logf("the API refused the probe token (HTTP 401): the probe was revoked or the token is wrong")

		return authBackoff
	case err != nil:
		p.logf("could not fetch monitors: %v", err)

		return errorBackoff
	}

	wait := time.Duration(list.PollSeconds) * time.Second
	if wait < pollMin || wait > pollMax {
		wait = pollDefault
	}

	if len(list.Monitors) == 0 {
		return wait
	}

	results := p.runAll(ctx, list.Monitors)

	for start := 0; start < len(results); start += resultBatch {
		end := min(start+resultBatch, len(results))

		if err := p.post(ctx, results[start:end]); err != nil {
			// Results are only worth something fresh: after a few tries the lease
			// runs out and the check is handed out again.
			p.logf("could not post %d result(s): %v", end-start, err)
		}
	}

	return wait
}

func (p *Probe) runAll(ctx context.Context, monitors []Monitor) []Result {
	fan := p.Concurrency
	if fan <= 0 {
		fan = defaultFanOut
	}

	results := make([]Result, len(monitors))
	slots := make(chan struct{}, fan)

	var wg sync.WaitGroup

	for i, monitor := range monitors {
		wg.Add(1)
		slots <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-slots }()

			results[i] = p.Checker.Run(ctx, monitor)
		}()
	}

	wg.Wait()

	return results
}

func (p *Probe) fetch(ctx context.Context) (listResponse, int, error) {
	var list listResponse

	address := p.Endpoint + "/api/v1/probe/monitors?version=" + url.QueryEscape(p.Version)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return list, 0, err
	}

	p.headers(request)

	response, err := p.Client.Do(request)
	if err != nil {
		return list, 0, errors.New("the API could not be reached")
	}

	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return list, response.StatusCode, fmt.Errorf("the API answered HTTP %d", response.StatusCode)
	}

	if err := json.NewDecoder(io.LimitReader(response.Body, maxListBytes)).Decode(&list); err != nil {
		return list, response.StatusCode, errors.New("the API's answer was not understood")
	}

	return list, response.StatusCode, nil
}

func (p *Probe) post(ctx context.Context, results []Result) error {
	body, err := json.Marshal(map[string]any{"results": results})
	if err != nil {
		return err
	}

	var last error

	for attempt := 1; attempt <= postAttempts; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint+"/api/v1/probe/results", bytes.NewReader(body))
		if err != nil {
			return err
		}

		p.headers(request)
		request.Header.Set("Content-Type", "application/json")

		response, err := p.Client.Do(request)

		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
			_ = response.Body.Close()

			switch {
			case response.StatusCode >= 200 && response.StatusCode < 300:
				return nil
			case response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests:
				return fmt.Errorf("the API refused the results (HTTP %d)", response.StatusCode)
			}

			last = fmt.Errorf("the API answered HTTP %d", response.StatusCode)
		} else {
			last = errors.New("the API could not be reached")
		}

		if attempt < postAttempts && !p.sleep(ctx, time.Duration(attempt)*2*time.Second) {
			return ctx.Err()
		}
	}

	return last
}

func (p *Probe) headers(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+p.Token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", p.UserAgent)
}

func (p *Probe) sleep(ctx context.Context, d time.Duration) bool {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (p *Probe) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}
