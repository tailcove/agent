package agent

import (
	"context"
	"os"
	"sync"

	"github.com/tailcove/agent/internal/config"
	"github.com/tailcove/agent/internal/docker"
	"github.com/tailcove/agent/internal/journald"
	"github.com/tailcove/agent/internal/source"
)

// startJournald follows the journal when it is switched on.
func (a *Agent) startJournald(ctx context.Context, emit func(source.Event), group *sync.WaitGroup) {
	settings := a.Config.Journald
	if !settings.Enabled {
		return
	}

	maxPriority, _ := journald.Priority(settings.MinPriority)

	reader := &journald.Reader{
		Path:          settings.Path,
		Directory:     settings.Directory,
		Units:         settings.Units,
		MaxPriority:   maxPriority,
		FromBeginning: settings.FromBeginning,
		Cursor:        a.State.Cursor(journald.Key),
		Host:          a.Config.Host,
		Emit:          emit,
		Logf:          a.logf,
	}

	group.Add(1)

	go func() {
		defer group.Done()
		reader.Run(ctx)
	}()
}

// startDocker follows the containers' logs when it is switched on. A socket that
// is missing or refused is said once and retried: Docker may start later than
// the agent.
func (a *Agent) startDocker(ctx context.Context, emit func(source.Event), group *sync.WaitGroup) {
	settings := a.Config.Docker
	if !settings.Enabled {
		return
	}

	multiline := settings.Multiline
	if multiline == nil {
		multiline = defaultMultiline()
	}

	joined, err := tailMultiline(multiline)
	if err != nil {
		a.logf("docker: ignoring the multiline setting: %v", err)
	}

	if _, err := os.Stat(settings.Socket); err != nil {
		a.logf("docker: %v (will keep trying)", err)
	}

	reader := &docker.Reader{
		Client:        docker.NewClient(settings.Socket),
		Host:          a.Config.Host,
		HostMode:      settings.Host,
		Labels:        settings.Labels,
		Include:       settings.Include,
		Exclude:       settings.Exclude,
		MaxContainers: settings.MaxContainers,
		FromBeginning: settings.FromBeginning,
		Multiline:     joined,
		Resume:        a.State.Cursor,
		Keys:          func() []string { return a.State.CursorKeys(docker.KeyPrefix) },
		Forget:        a.State.ForgetCursor,
		Emit:          emit,
		Logf:          a.logf,
		Now:           a.Now,
	}

	group.Add(1)

	go func() {
		defer group.Done()
		reader.Run(ctx)
	}()
}

// defaultMultiline joins stack traces unless a configuration says otherwise.
func defaultMultiline() *config.Multiline {
	return &config.Multiline{Mode: "auto"}
}
