// Package state persists the small amount of dspool state that must survive a
// restart: the tombstone list of upstream keys DeepSeek rejected.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const version = 1

type InvalidKey struct {
	Label  string    `json:"label"`
	Mask   string    `json:"mask"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

type data struct {
	Version int                   `json:"version"`
	Invalid map[string]InvalidKey `json:"invalid,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	path string
	log  *slog.Logger
	data data
}

func Open(path string, log *slog.Logger) (*Store, error) {
	s := &Store{
		path: path,
		log:  log,
		data: data{Version: version, Invalid: map[string]InvalidKey{}},
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, fmt.Errorf("parse state file %s: %w", path, err)
		}
		if s.data.Version != version {
			return nil, fmt.Errorf("state file %s has unsupported version %d", path, s.data.Version)
		}
		if s.data.Invalid == nil {
			s.data.Invalid = map[string]InvalidKey{}
		}
	case errors.Is(err, os.ErrNotExist):
		// Fresh install.
	default:
		return nil, fmt.Errorf("read state file %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) IsInvalid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data.Invalid[id]
	return ok
}

func (s *Store) MarkInvalid(id, label, mask, reason string, at time.Time) {
	s.mu.Lock()
	s.data.Invalid[id] = InvalidKey{Label: label, Mask: mask, Reason: reason, Since: at}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		s.log.Error("failed to persist invalid key tombstone", "error", err)
	}
}

func (s *Store) ClearInvalid(id string) {
	s.mu.Lock()
	if _, ok := s.data.Invalid[id]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.data.Invalid, id)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		s.log.Error("failed to persist tombstone removal", "error", err)
	}
}

// InvalidKeys returns a copy of the tombstone map for the admin endpoint.
func (s *Store) InvalidKeys() map[string]InvalidKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]InvalidKey, len(s.data.Invalid))
	for id, v := range s.data.Invalid {
		out[id] = v
	}
	return out
}

// Flush writes the current state if a file is configured.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
