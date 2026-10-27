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
	cursors   map[string]string
	dirty     bool
}

// stateFile is what is written. Older agents wrote the positions alone, as a
// map from path to position; that shape is still read.
type stateFile struct {
	Positions map[string]tail.Position `json:"positions"`
	Cursors   map[string]string        `json:"cursors,omitempty"`
}

// LoadState reads the state file. A missing or unreadable file is an empty
// state: the agent then follows files from their ends.
func LoadState(path string) *State {
	state := &State{path: path, positions: map[string]tail.Position{}, cursors: map[string]string{}}

	if path == "" {
		return state
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return state
	}

	var current stateFile

	if json.Unmarshal(raw, &current) == nil && current.Positions != nil {
		state.positions = current.Positions

		if current.Cursors != nil {
			state.cursors = current.Cursors
		}

		return state
	}

	var legacy map[string]tail.Position
	if json.Unmarshal(raw, &legacy) == nil && legacy != nil {
		state.positions = legacy
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

// Cursor is where the stream called key (journald, one container) was last
// delivered to, or "" when it is not known.
func (s *State) Cursor(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cursors[key]
}

// RecordCursor notes that a stream was delivered up to cursor.
func (s *State) RecordCursor(key, cursor string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cursors[key] == cursor {
		return
	}

	s.cursors[key] = cursor
	s.dirty = true
}

// ForgetCursor drops the cursor of a stream that is gone (a removed container).
func (s *State) ForgetCursor(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, known := s.cursors[key]; known {
		delete(s.cursors, key)
		s.dirty = true
	}
}

// CursorKeys lists the streams that have a cursor and whose key starts with prefix.
func (s *State) CursorKeys(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var keys []string

	for key := range s.cursors {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}

	return keys
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

	encoded, err := json.Marshal(stateFile{Positions: s.positions, Cursors: s.cursors})
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
