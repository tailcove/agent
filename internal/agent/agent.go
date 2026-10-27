// Package agent wires the pieces together: it follows log files, collects host
// metrics, and delivers both to Tailcove.
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/tailcove/agent/internal/config"
	"github.com/tailcove/agent/internal/hostmetrics"
	"github.com/tailcove/agent/internal/sender"
	"github.com/tailcove/agent/internal/tail"
)

// item is a line ready to send, with where in its file it ended.
type item struct {
	file     string
	position tail.Position
	entry    json.RawMessage
}

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

	// Kube is set when Kubernetes discovery is on (see NewKubernetes).
	Kube *Kubernetes
}

// Run starts the log and metric pipelines and returns when both have stopped.
// On shutdown the lines already read are delivered (within the context that
// Run was given: cancel it to stop sooner).
func (a *Agent) Run(ctx context.Context) {
	if a.Now == nil {
		a.Now = time.Now
	}

	var group sync.WaitGroup

	if len(a.Config.Logs) > 0 || a.Kube != nil {
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

	if a.Kube != nil && a.Config.Kubernetes.MetricsEnabled() {
		group.Add(1)

		go func() {
			defer group.Done()
			a.runKubeMetrics(ctx)
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
	sources := make([]tail.Source, 0, len(a.Config.Logs)+1)

	for _, source := range a.Config.Logs {
		multiline, err := tailMultiline(source.Multiline)
		if err != nil {
			a.logf("ignoring the multiline setting of %s: %v", source.Path, err)
		}

		sources = append(sources, tail.Source{Pattern: source.Path, FromBeginning: source.FromBeginning, Multiline: multiline})
	}

	if a.Kube != nil {
		sources = append(sources, a.Kube.Source())
	}

	lines := make(chan tail.Line, lineQueueLength)
	follower := &tail.Follower{Sources: sources, Resume: a.State.Positions(), Lines: lines, Gone: a.forgetFile}

	followed := make(chan struct{})

	go func() {
		follower.Run(ctx)
		close(followed)
	}()

	a.routeLines(ctx, lines, followed)
}

// forgetFile drops what is saved of a file that was removed (a pod that was
// deleted): nothing will be read from it again.
func (a *Agent) forgetFile(path string) {
	a.State.Forget(path)
}

// sink is where the lines of one source go: its own queue, its own sender and
// its own batches, so a source whose token is refused can stall nobody else.
type sink struct {
	items   chan item
	sender  *sender.Sender
	dropped bool
}

// routeLines reads what the follower found and hands each line to the sink of
// the source it belongs to: the main one, or the one a Kubernetes route names.
// The main sink applies backpressure (reading slows down until it catches up);
// a routed sink that cannot keep up drops what does not fit, says so once, and
// the dropped lines are read again at the next start (their file position was
// never recorded).
func (a *Agent) routeLines(ctx context.Context, lines <-chan tail.Line, followed <-chan struct{}) {
	var (
		group sync.WaitGroup
		sinks = map[string]*sink{}
	)

	done := make(chan struct{})

	open := func(token string) *sink {
		if existing, found := sinks[token]; found {
			return existing
		}

		target := a.Sender
		if token != "" {
			target = a.Sender.WithToken(token)
		}

		created := &sink{items: make(chan item, lineQueueLength), sender: target}
		sinks[token] = created

		group.Add(1)

		go func() {
			defer group.Done()
			a.batchLines(ctx, created.sender, created.items, done)
		}()

		return created
	}

	open("")

	deliver := func(line tail.Line) {
		entry, token := a.encode(ctx, line)
		target := open(token)
		queued := item{file: line.File, position: tail.Position{Inode: line.Inode, Offset: line.Offset}, entry: entry}

		if token == "" {
			select {
			case target.items <- queued:
			case <-ctx.Done():
			}

			return
		}

		select {
		case target.items <- queued:
		default:
			if !target.dropped {
				target.dropped = true
				a.logf("a Kubernetes route cannot keep up (is its token refused?): its lines are dropped until it does, and read again after a restart")
			}
		}
	}

	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()

	for {
		select {
		case line := <-lines:
			deliver(line)
		case <-prune.C:
			if a.Kube != nil {
				a.State.PruneMissing(strings.TrimRight(a.Kube.PodsDir(), "/") + "/")
			}
		case <-followed:
			// Reading has stopped: pass on what is queued behind it, then let
			// every sink deliver what it holds.
			for drained := false; !drained; {
				select {
				case line := <-lines:
					deliver(line)
				default:
					drained = true
				}
			}

			close(done)
			group.Wait()

			return
		}
	}
}

// encode turns a line into the entry to send and says which source it is for
// ("" is the main one).
func (a *Agent) encode(ctx context.Context, line tail.Line) (json.RawMessage, string) {
	if a.Kube != nil {
		if entry, token, ok := a.Kube.Encode(ctx, line, a.Config.Host); ok {
			return entry, token
		}
	}

	return logEntry(line, a.Config.Host), ""
}

// batchLines groups lines into requests and records how far each file has been
// delivered once its batch is accepted (or refused for good). It returns when
// done is closed and what was queued has been delivered.
func (a *Agent) batchLines(ctx context.Context, post *sender.Sender, items <-chan item, done <-chan struct{}) {
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

		outcome := post.Post(deliverCtx, logsPath, logsPayload(entries))

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
		case queued := <-items:
			entries = append(entries, queued.entry)
			positions[queued.file] = queued.position
			size += len(queued.entry)

			if len(entries) >= maxBatchLines || size >= maxBatchBytes {
				flush(ctx)
			}
		case <-timer.C:
			flush(ctx)
		case <-saver.C:
			a.saveState()
		case <-done:
			// Reading has stopped: deliver what was read, within a short grace
			// period, and what is still queued behind it.
			grace, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			for {
				select {
				case queued := <-items:
					entries = append(entries, queued.entry)
					positions[queued.file] = queued.position

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

	collector := hostmetrics.NewCollectorAt(a.Config.Metrics.ProcRoot, exclude, a.Config.Metrics.ExcludeMounts)
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
