package configmigrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// StoredRelease preserves one complete, successful migration transaction.
type StoredRelease struct {
	Original Document `json:"original"`
	Document Document `json:"document"`
	Path     []Record `json:"path"`
}

type storedState struct {
	Document Document        `json:"document"`
	History  []StoredRelease `json:"history"`
}

// JSONStore atomically publishes complete configuration states to a single JSON file.
// The original input, migrated output, and complete migration path are all preserved in history.
type JSONStore struct {
	path string
}

// NewJSONStore creates a store whose current state is located at path.
func NewJSONStore(path string) *JSONStore {
	return &JSONStore{path: path}
}

// Load reads the currently published document. It returns ErrNotFound before the first publish.
func (s *JSONStore) Load() (Document, []StoredRelease, error) {
	contents, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	var state storedState
	if err := json.Unmarshal(contents, &state); err != nil {
		return nil, nil, fmt.Errorf("decode configuration state: %w", err)
	}
	return state.Document, state.History, nil
}

// Publish returns a callback suitable for Engine.Publish.
func (s *JSONStore) Publish() func(context.Context, Release) error {
	return func(_ context.Context, release Release) error {
		return s.Commit(release)
	}
}

// Commit writes the next state to a temporary file and renames it over the current state.
// It is safe to call directly, but failed migrations should never call it.
func (s *JSONStore) Commit(release Release) error {
	state := storedState{
		Document: release.Document,
		History: []StoredRelease{{
			Original: release.Original,
			Document: release.Document,
			Path:     release.Path,
		}},
	}
	if _, history, err := s.Load(); err == nil {
		state.History = append(history, state.History...)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	temp, err := os.CreateTemp(dir, ".config-state-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if _, err := temp.Write(append(contents, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, s.path)
}
