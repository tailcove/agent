// Package agent wires the pieces together: it follows log files, collects host
// metrics, and delivers both to Tailcove.
package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/tailcove/agent/internal/config"
	"github.com/tailcove/agent/internal/hostmetrics"
	"github.com/tailcove/agent/internal/sender"
	"github.com/tailcove/agent/internal/tail"
)

const (
	maxBatchLines   = 500
	maxBatchBytes   = 1 << 20
	flushInterval   = time.Second
	pointsPerPost   = 500
	logsPath        = "/api/v1/logs"
	metricsPath     = "/api/v1/metrics"
	stateSaveEvery  = 5 * time.Second
	lineQueueLength = 2000
)

// Agent runs until its context ends.
type Agent struct {
	Config config.Config
	Sender *sender.Sender
	State  *State
	Logf   func(format string, args ...any)

	// Now is the clock; tests replace it.
	Now func() time.Time
}

// Run starts the log and metric pipelines and returns when both have stopped.
// On shutdown the lines already read are delivered (within the context that
// Run was given: cancel it to stop sooner).
func (a *Agent) Run(ctx context.Context) {
	if a.Now == nil {
		a.Now = time.Now
	}

	var group sync.WaitGroup

	if len(a.Config.Logs) > 0 {
		group.Add(1)

		go func() {
			defer group.Done()
			a.runLogs(ctx)
		}()
	}

	if a.Config.MetricsEnabled() {
		group.Add(1)

		go func() {
			defer group.Done()
			a.runMetrics(ctx)
		}()
	}

	group.Wait()
}

func (a *Agent) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

func (a *Agent) runLogs(ctx context.Context) {
	sources := make([]tail.Source, 0, len(a.Config.Logs))

	for _, source := range a.Config.Logs {
		sources = append(sources, tail.Source{Pattern: source.Path, FromBeginning: source.FromBeginning})
	}

	lines := make(chan tail.Line, lineQueueLength)
	follower := &tail.Follower{Sources: sources, Resume: a.State.Positions(), Lines: lines}

	followed := make(chan struct{})

	go func() {
		follower.Run(ctx)
		close(followed)
	}()

	a.batchLines(ctx, lines, followed)
}

// batchLines groups lines into requests and records how far each file has been
// delivered once its batch is accepted (or refused for good).
func (a *Agent) batchLines(ctx context.Context, lines <-chan tail.Line, followed <-chan struct{}) {
	var (
		entries   []json.RawMessage
		positions = map[string]tail.Position{}
		size      int
	)

	timer := time.NewTicker(flushInterval)
	saver := time.NewTicker(stateSaveEvery)

	defer timer.Stop()
	defer saver.Stop()

	flush := func(deliverCtx context.Context) {
		if len(entries) == 0 {
			return
		}

		outcome := a.Sender.Post(deliverCtx, logsPath, logsPayload(entries))

		// A batch that was not delivered because the agent is stopping stays
		// queued for the shutdown flush; its lines are sent again on the next
		// start at the latest, because the file position only moves on delivery.
		if outcome == sender.Cancelled {
			return
		}

		for file, position := range positions {
			a.State.Record(file, position)
		}

		entries, positions, size = nil, map[string]tail.Position{}, 0
	}

	for {
		select {
		case line := <-lines:
			entry := logEntry(line, a.Config.Host)
			entries = append(entries, entry)
			positions[line.File] = tail.Position{Inode: line.Inode, Offset: line.Offset}
			size += len(entry)

			if len(entries) >= maxBatchLines || size >= maxBatchBytes {
				flush(ctx)
			}
		case <-timer.C:
			flush(ctx)
		case <-saver.C:
			a.saveState()
		case <-followed:
			// Reading has stopped: deliver what was read, within a short grace
			// period, and what is still queued behind it.
			grace, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			for {
				select {
				case line := <-lines:
					entries = append(entries, logEntry(line, a.Config.Host))
					positions[line.File] = tail.Position{Inode: line.Inode, Offset: line.Offset}

					if len(entries) >= maxBatchLines {
						flush(grace)
					}
				default:
					flush(grace)
					a.saveState()

					return
				}
			}
		}
	}
}

func (a *Agent) saveState() {
	if err := a.State.Save(); err != nil {
		a.logf("could not save the state file: %v", err)
	}
}

func (a *Agent) runMetrics(ctx context.Context) {
	exclude := a.Config.Metrics.ExcludeIfaces
	if exclude == nil {
		exclude = hostmetrics.DefaultExcludedInterfaces
	}

	collector := hostmetrics.NewCollector(exclude, a.Config.Metrics.ExcludeMounts)
	seconds := a.Config.Metrics.IntervalSeconds
	if seconds <= 0 {
		seconds = config.DefaultMetricsInterval
	}

	interval := time.Duration(seconds) * time.Second

	// Reads once straight away so the first interval has a CPU reading to compare with.
	collector.Collect()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := a.Now()

			// A reading that cannot be delivered within an interval is not worth
			// delivering late: the next one replaces it.
			deliver, cancel := context.WithTimeout(ctx, interval)

			for _, payload := range metricsPayloads(collector.Collect(), a.Config.Host, now, pointsPerPost) {
				a.Sender.Post(deliver, metricsPath, payload)
			}

			cancel()
		}
	}
}
