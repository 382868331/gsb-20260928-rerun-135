package configmigrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// MergeWorkingState is the complete state of an ongoing merge: the common
// source, both original candidates, the declared origins and references,
// and every human decision recorded so far.
type MergeWorkingState struct {
	Base       Document          `json:"base"`
	A          Document          `json:"a"`
	B          Document          `json:"b"`
	OriginA    map[string]string `json:"origin_a,omitempty"`
	OriginB    map[string]string `json:"origin_b,omitempty"`
	References []MergeReference  `json:"references,omitempty"`
	Decisions  []MergeDecision   `json:"decisions,omitempty"`
}

// Options returns the correlation options carried by the working state.
func (s MergeWorkingState) Options() MergeOptions {
	return MergeOptions{OriginA: s.OriginA, OriginB: s.OriginB, References: s.References}
}

// Clone returns an independent copy of the working state.
func (s MergeWorkingState) Clone() MergeWorkingState {
	clone := MergeWorkingState{
		Base: s.Base.Clone(),
		A:    s.A.Clone(),
		B:    s.B.Clone(),
	}
	if s.OriginA != nil {
		clone.OriginA = make(map[string]string, len(s.OriginA))
		for key, value := range s.OriginA {
			clone.OriginA[key] = value
		}
	}
	if s.OriginB != nil {
		clone.OriginB = make(map[string]string, len(s.OriginB))
		for key, value := range s.OriginB {
			clone.OriginB[key] = value
		}
	}
	clone.References = append([]MergeReference(nil), s.References...)
	clone.Decisions = append([]MergeDecision(nil), s.Decisions...)
	return clone
}

// MergeHistoryEntry is one append-only audit record. Saves, adoptions, and
// undos are all kept, so the original candidates and every human decision
// remain queryable even after an undo.
type MergeHistoryEntry struct {
	Seq       int               `json:"seq"`
	Timestamp time.Time         `json:"timestamp"`
	Action    string            `json:"action"`
	State     MergeWorkingState `json:"state"`
	Release   *MergeRelease     `json:"release,omitempty"`
}

// MergeStore persists a merge session to a single JSON file. Writes go
// through a temporary file plus atomic rename, so a reopened store always
// sees a complete state. History is append-only: undo restores a previous
// working state as the current one without deleting anything.
type MergeStore struct {
	path string
}

// NewMergeStore creates a store whose state lives at path.
func NewMergeStore(path string) *MergeStore {
	return &MergeStore{path: path}
}

type storedMergeState struct {
	Current MergeWorkingState   `json:"current"`
	History []MergeHistoryEntry `json:"history"`
}

// Load returns the current working state and the full audit history. It
// returns ErrNotFound before the first save.
func (s *MergeStore) Load() (MergeWorkingState, []MergeHistoryEntry, error) {
	contents, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return MergeWorkingState{}, nil, ErrNotFound
	}
	if err != nil {
		return MergeWorkingState{}, nil, err
	}
	var state storedMergeState
	if err := json.Unmarshal(contents, &state); err != nil {
		return MergeWorkingState{}, nil, fmt.Errorf("decode merge state: %w", err)
	}
	return state.Current.Clone(), state.History, nil
}

func (s *MergeStore) commit(action string, current MergeWorkingState, release *MergeRelease) error {
	_, history, err := s.Load()
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	history = append(history, MergeHistoryEntry{
		Seq:       len(history) + 1,
		Timestamp: timeNow().UTC(),
		Action:    action,
		State:     current.Clone(),
		Release:   release,
	})
	state := storedMergeState{Current: current.Clone(), History: history}
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".merge-state-*.tmp")
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

// Save records the current working state so a later session can reopen it.
// The previous state stays in history.
func (s *MergeStore) Save(state MergeWorkingState) error {
	return s.commit("save", state, nil)
}

// Publish returns a callback suitable for AdoptMerge. The adopted release,
// including the original candidates and the decisions behind it, is stored
// alongside the working state.
func (s *MergeStore) Publish(state MergeWorkingState) func(context.Context, MergeRelease) error {
	return func(_ context.Context, release MergeRelease) error {
		return s.commit("adopt", state, &release)
	}
}

// Undo restores the working state from before the most recent history
// entry. The undo itself is appended to history, so no candidate or
// decision is ever lost. It returns ErrNotFound when there is nothing to
// undo to.
func (s *MergeStore) Undo() (MergeWorkingState, error) {
	_, history, err := s.Load()
	if err != nil {
		return MergeWorkingState{}, err
	}
	if len(history) < 2 {
		return MergeWorkingState{}, fmt.Errorf("%w: no earlier merge state to undo to", ErrNotFound)
	}
	previous := history[len(history)-2].State.Clone()
	if err := s.commit("undo", previous, nil); err != nil {
		return MergeWorkingState{}, err
	}
	return previous, nil
}
