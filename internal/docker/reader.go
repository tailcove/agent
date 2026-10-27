package docker

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tailcove/agent/internal/source"
	"github.com/tailcove/agent/internal/tail"
)

// KeyPrefix starts the name of a container's cursor in the state file.
const KeyPrefix = "docker:"

const (
	composeProject = "com.docker.compose.project"
	composeService = "com.docker.compose.service"
)

// Logs is what Reader needs of the engine; Client is the real one.
type Logs interface {
	List(ctx context.Context) ([]Container, error)
	Tty(ctx context.Context, id string) (bool, error)
	Logs(ctx context.Context, id string, since time.Time) (io.ReadCloser, error)
}

// Reader follows the logs of the running containers: each one gets a stream of
// its own, started when the container appears and ended when it stops.
type Reader struct {
	Client Logs
	// Host is the name of this machine, for HostMode "host".
	Host string
	// HostMode is what an entry's host is: "container" (default), "host" (this machine), or "service" (the Compose service, else the container).
	HostMode string
	// Labels are the container labels kept in an entry's context, besides the Compose project and service.
	Labels []string
	// Include and Exclude are globs on the container name; when Include is set, only matching containers are followed.
	Include []string
	Exclude []string
	// MaxContainers bounds the streams that are open at once.
	MaxContainers int
	// FromBeginning reads a container's whole log the first time it is seen; otherwise only what is written from then on.
	FromBeginning bool
	// Multiline joins the lines of one event (a stack trace).
	Multiline *tail.Multiline
	// Resume returns the cursor saved for a stream key, or "".
	Resume func(key string) string
	// Keys lists the stream keys that have a saved cursor, and Forget drops one: a
	// container that is gone for good must not keep its cursor for ever.
	Keys   func() []string
	Forget func(key string)

	Emit func(source.Event)
	Logf func(format string, args ...any)

	// Interval is how often the container list is read; Now is the clock.
	Interval time.Duration
	Now      func() time.Time

	mu        sync.Mutex
	following map[string]bool
	missing   map[string]int
	warned    map[string]bool
}

// Accept says whether a container's logs are read.
func (r *Reader) Accept(c Container) bool {
	if len(r.Include) > 0 && !matchesAny(r.Include, c.Name) {
		return false
	}

	return !matchesAny(r.Exclude, c.Name)
}

func matchesAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if ok, err := filepath.Match(pattern, name); err == nil && ok {
			return true
		}
	}

	return false
}

// Run follows the containers until the context ends.
func (r *Reader) Run(ctx context.Context) {
	if r.Interval <= 0 {
		r.Interval = 10 * time.Second
	}

	if r.Now == nil {
		r.Now = time.Now
	}

	if r.MaxContainers <= 0 {
		r.MaxContainers = 200
	}

	r.following = map[string]bool{}
	r.missing = map[string]int{}
	r.warned = map[string]bool{}

	var group sync.WaitGroup

	defer group.Wait()

	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	for {
		r.scan(ctx, &group)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// scan starts a stream for each accepted container that has none, and forgets
// the cursor of one that has been missing for several scans after its stream ended.
func (r *Reader) scan(ctx context.Context, group *sync.WaitGroup) {
	containers, err := r.Client.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.warnOnce("list", "docker: cannot list the containers: %v", err)
		}

		return
	}

	delete(r.warned, "list")

	present := map[string]bool{}

	for _, container := range containers {
		present[container.ID] = true

		if !r.Accept(container) {
			continue
		}

		r.mu.Lock()
		busy, open := r.following[container.ID], len(r.following)
		r.mu.Unlock()

		if busy {
			continue
		}

		if open >= r.MaxContainers {
			r.warnOnce("max", "docker: %d containers are followed already (max_containers); the others are left out", r.MaxContainers)

			continue
		}

		r.mu.Lock()
		r.following[container.ID] = true
		r.mu.Unlock()

		group.Add(1)

		go func(container Container) {
			defer group.Done()

			r.follow(ctx, container)

			r.mu.Lock()
			delete(r.following, container.ID)
			r.mu.Unlock()
		}(container)
	}

	r.forgetGone(present)
}

// forgetGone drops the cursor of every container that has not been listed for
// three scans in a row and has no stream open.
func (r *Reader) forgetGone(present map[string]bool) {
	if r.Forget == nil || r.Keys == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, key := range r.Keys() {
		id := strings.TrimPrefix(key, KeyPrefix)

		if present[id] || r.following[id] {
			delete(r.missing, id)

			continue
		}

		r.missing[id]++

		if r.missing[id] >= 3 {
			delete(r.missing, id)
			r.Forget(key)
		}
	}
}

func (r *Reader) warnOnce(key, format string, args ...any) {
	if r.warned[key] || r.Logf == nil {
		return
	}

	r.warned[key] = true
	r.Logf(format, args...)
}

// follow reads one container's log until it ends, and emits what it finds.
func (r *Reader) follow(ctx context.Context, container Container) {
	key := KeyPrefix + container.ID
	cursor := time.Time{}

	if r.Resume != nil {
		if saved := r.Resume(key); saved != "" {
			if at, err := time.Parse(time.RFC3339Nano, saved); err == nil {
				cursor = at
			}
		}
	}

	since := cursor

	if cursor.IsZero() && !r.FromBeginning {
		since = r.Now()
	}

	tty, err := r.Client.Tty(ctx, container.ID)
	if err != nil {
		if ctx.Err() == nil {
			r.logf("docker: cannot inspect %s: %v", container.Name, err)
		}

		return
	}

	stream, err := r.Client.Logs(ctx, container.ID, since)
	if err != nil {
		if ctx.Err() == nil {
			r.logf("docker: cannot read the logs of %s: %v", container.Name, err)
		}

		return
	}

	defer func() { _ = stream.Close() }()

	follower := &containerStream{reader: r, container: container, key: key, resumeAfter: cursor, joiners: map[string]*tail.Joiner{}, lines: NewLines()}

	// A context that ends closes the stream, which ends the read below.
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()

	idle := time.NewTicker(250 * time.Millisecond)
	defer idle.Stop()

	finished := make(chan struct{})

	go func() {
		defer close(finished)

		if err := Demux(stream, tty, follower.chunk); err != nil && ctx.Err() == nil {
			r.logf("docker: the log stream of %s ended: %v", container.Name, err)
		}
	}()

	for {
		select {
		case <-finished:
			follower.finish()

			return
		case <-idle.C:
			follower.idle(r.Now())
		}
	}
}

func (r *Reader) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// containerStream is the state of one container's stream: the line splitter,
// one joiner per output stream, and how far its cursor has come.
type containerStream struct {
	reader    *Reader
	container Container
	key       string
	// resumeAfter is where the last run stopped: lines up to it were delivered then and are not sent again.
	resumeAfter time.Time

	mu      sync.Mutex
	lines   *Lines
	joiners map[string]*tail.Joiner
}

func (s *containerStream) chunk(chunk Chunk) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.reader.Now()

	for _, line := range s.lines.Add(chunk) {
		// The engine's `since` is not exact to the nanosecond: what was handled before the restart is not handled twice.
		if !s.resumeAfter.IsZero() && !line.Time.After(s.resumeAfter) {
			continue
		}

		joiner := s.joiners[line.Stream]
		if joiner == nil {
			joiner = tail.NewJoiner(s.reader.Multiline)
			s.joiners[line.Stream] = joiner
		}

		for _, done := range joiner.Add(line.Text, line.Time, now, line.Stream) {
			s.emit(done)
		}
	}
}

func (s *containerStream) idle(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, joiner := range s.joiners {
		if done, ok := joiner.Idle(now); ok {
			s.emit(done)
		}
	}
}

// finish sends what is still open when the stream ends.
func (s *containerStream) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, joiner := range s.joiners {
		if done, ok := joiner.Flush(); ok {
			s.emit(done)
		}
	}
}

// emit sends an event. Its cursor is the time of its last line, but never past
// the first line of an event that is still being joined on the other stream:
// the cursor must not move beyond anything that has not been sent.
func (s *containerStream) emit(done tail.Joined) {
	cursor := done.Last

	for _, joiner := range s.joiners {
		if since, open := joiner.OpenSince(); open && !since.After(cursor) {
			cursor = since.Add(-time.Nanosecond)
		}
	}

	if s.reader.Emit == nil {
		return
	}

	s.reader.Emit(source.Event{
		Entry:  s.reader.entry(s.container, done),
		Key:    s.key,
		Cursor: cursor.UTC().Format(time.RFC3339Nano),
	})
}

// entry is the JSON object for one event. An event that is already a JSON object
// is passed on as it is, with the host, a time and the container added; any
// other text becomes a message.
func (r *Reader) entry(container Container, done tail.Joined) json.RawMessage {
	host := r.hostFor(container)
	docker := r.context(container, done.Stream)
	text := strings.TrimSpace(done.Text)

	if strings.HasPrefix(text, "{") && strings.HasSuffix(text, "}") {
		var object map[string]any

		if err := json.Unmarshal([]byte(text), &object); err == nil {
			if _, named := object["host"]; !named {
				if _, named := object["hostname"]; !named {
					object["host"] = host
				}
			}

			dated := false

			for _, key := range []string{"ts", "timestamp", "time", "@timestamp", "datetime", "date"} {
				if _, named := object[key]; named {
					dated = true

					break
				}
			}

			if !dated {
				object["ts"] = done.Time.UTC().Format(time.RFC3339Nano)
			}

			switch context := object["context"].(type) {
			case nil:
				object["context"] = map[string]any{"docker": docker}
			case map[string]any:
				if _, named := context["docker"]; !named {
					context["docker"] = docker
				}
			}

			if encoded, err := json.Marshal(object); err == nil {
				return encoded
			}
		}
	}

	encoded, _ := json.Marshal(map[string]any{
		"message": done.Text,
		"ts":      done.Time.UTC().Format(time.RFC3339Nano),
		"host":    host,
		"context": map[string]any{"docker": docker},
	})

	return encoded
}

func (r *Reader) hostFor(container Container) string {
	switch r.HostMode {
	case "host":
		if r.Host != "" {
			return r.Host
		}
	case "service":
		if service := container.Labels[composeService]; service != "" {
			return service
		}
	}

	if container.Name != "" {
		return container.Name
	}

	return r.Host
}

func (r *Reader) context(container Container, stream string) map[string]any {
	docker := map[string]any{
		"container": container.Name,
		"id":        container.ID[:12],
		"image":     container.Image,
	}

	if stream != "" {
		docker["stream"] = stream
	}

	if project := container.Labels[composeProject]; project != "" {
		docker["compose_project"] = project
	}

	if service := container.Labels[composeService]; service != "" {
		docker["compose_service"] = service
	}

	labels := map[string]string{}

	for _, key := range r.Labels {
		if value, ok := container.Labels[key]; ok {
			labels[key] = value
		}
	}

	if len(labels) > 0 {
		docker["labels"] = labels
	}

	return docker
}
