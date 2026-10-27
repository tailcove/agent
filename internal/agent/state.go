package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tailcove/agent/internal/tail"
)

// State remembers how far each followed file has been delivered, so a restart
// carries on instead of sending lines twice or skipping the ones written while
// the agent was down.
type State struct {
	path string

	mu        sync.Mutex
	positions map[string]tail.Position
	dirty     bool
}

// LoadState reads the state file. A missing or unreadable file is an empty
// state: the agent then follows files from their ends.
func LoadState(path string) *State {
	state := &State{path: path, positions: map[string]tail.Position{}}

	if path == "" {
		return state
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return state
	}

	var saved map[string]tail.Position
	if json.Unmarshal(raw, &saved) == nil && saved != nil {
		state.positions = saved
	}

	return state
}

// Positions is a copy of what is saved, for tail.Follower.Resume.
func (s *State) Positions() map[string]tail.Position {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := make(map[string]tail.Position, len(s.positions))
	for path, position := range s.positions {
		copied[path] = position
	}

	return copied
}

// Record notes a delivered position (it never moves one backwards within a file).
func (s *State) Record(file string, position tail.Position) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if current, known := s.positions[file]; known && current.Inode == position.Inode && current.Offset >= position.Offset {
		return
	}

	s.positions[file] = position
	s.dirty = true
}

// Save writes the state if it changed: to a temporary file in the same
// directory, then renamed over the old one, so a crash never leaves half a file.
func (s *State) Save() error {
	if s.path == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dirty {
		return nil
	}

	encoded, err := json.Marshal(s.positions)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return err
	}

	_, writeErr := temporary.Write(encoded)
	closeErr := temporary.Close()

	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(temporary.Name())

		return err
	}

	if err := os.Rename(temporary.Name(), s.path); err != nil {
		_ = os.Remove(temporary.Name())

		return err
	}

	s.dirty = false

	return nil
}

// Forget drops the saved position of a file that is gone.
func (s *State) Forget(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, known := s.positions[path]; known {
		delete(s.positions, path)
		s.dirty = true
	}
}

// PruneMissing drops the positions of files under prefix that no longer exist
// and returns how many. A pod that is deleted while the agent is down leaves its
// position behind, and so does a line still in flight when its file went.
func (s *State) PruneMissing(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0

	for path := range s.positions {
		if !strings.HasPrefix(path, prefix) {
			continue
		}

		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			delete(s.positions, path)
			removed++
		}
	}

	if removed > 0 {
		s.dirty = true
	}

	return removed
}
