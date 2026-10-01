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

// StoredGroupRelease preserves one complete, successful group transaction,
// including the pre-migration originals.
type StoredGroupRelease struct {
	ID                 string              `json:"id"`
	Timestamp          time.Time           `json:"timestamp"`
	Originals          map[string]Document `json:"originals"`
	Documents          map[string]Document `json:"documents"`
	Targets            map[string]string   `json:"targets"`
	Records            map[string][]Record `json:"records,omitempty"`
	References         []ReferencePreview  `json:"references,omitempty"`
	PreviewFingerprint string              `json:"preview_fingerprint,omitempty"`
}

// StoredGroupPreview preserves a confirmed preview so the basis of a past
// decision remains queryable after restarts.
type StoredGroupPreview struct {
	Timestamp time.Time    `json:"timestamp"`
	Preview   GroupPreview `json:"preview"`
}

type storedGroupState struct {
	Documents map[string]Document  `json:"documents"`
	ReleaseID string               `json:"release_id,omitempty"`
	History   []StoredGroupRelease `json:"history,omitempty"`
	Previews  []StoredGroupPreview `json:"previews,omitempty"`
}

// GroupStore atomically publishes whole configuration groups to a single
// state file. A crash can only leave the complete old group or the complete
// new group behind; originals, release history, and confirmed previews are
// retained in the same file.
type GroupStore struct {
	path string
	now  func() time.Time
}

// NewGroupStore creates a group store whose state is located at path.
func NewGroupStore(path string) *GroupStore {
	return &GroupStore{path: path, now: timeNow}
}

func (s *GroupStore) readState() (storedGroupState, error) {
	contents, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return storedGroupState{}, ErrNotFound
	}
	if err != nil {
		return storedGroupState{}, err
	}
	var state storedGroupState
	if err := json.Unmarshal(contents, &state); err != nil {
		return storedGroupState{}, fmt.Errorf("decode group state: %w", err)
	}
	return state, nil
}

func (s *GroupStore) writeState(state storedGroupState) error {
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	temp, err := os.CreateTemp(dir, ".group-state-*.tmp")
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

// Load reads the currently published group. It returns ErrNotFound before
// the first publish.
func (s *GroupStore) Load() (Group, []StoredGroupRelease, error) {
	state, err := s.readState()
	if err != nil {
		return nil, nil, err
	}
	return Group(state.Documents).Clone(), state.History, nil
}

// Previews returns every recorded group preview, newest first.
func (s *GroupStore) Previews() ([]StoredGroupPreview, error) {
	state, err := s.readState()
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return state.Previews, nil
}

// RecordPreview persists a confirmed group preview without changing the
// published group, so the basis of a decision survives restarts.
func (s *GroupStore) RecordPreview(preview GroupPreview) error {
	state, err := s.readState()
	if errors.Is(err, ErrNotFound) {
		state = storedGroupState{}
	} else if err != nil {
		return err
	}
	state.Previews = append([]StoredGroupPreview{{Timestamp: s.now().UTC(), Preview: preview}}, state.Previews...)
	return s.writeState(state)
}

// PublishGroup returns a callback suitable for Engine.ApplyGroup.
func (s *GroupStore) PublishGroup() func(context.Context, GroupRelease) error {
	return func(_ context.Context, release GroupRelease) error {
		return s.Commit(release)
	}
}

// Commit atomically replaces the current group with the fully migrated one
// and prepends the release (including originals) to the history.
func (s *GroupStore) Commit(release GroupRelease) error {
	state, err := s.readState()
	if errors.Is(err, ErrNotFound) {
		state = storedGroupState{}
	} else if err != nil {
		return err
	}
	entry := StoredGroupRelease{
		ID:                 fmt.Sprintf("rel-%d", s.now().UTC().UnixNano()),
		Timestamp:          s.now().UTC(),
		Originals:          release.Originals,
		Documents:          release.Documents,
		Targets:            release.Targets,
		Records:            release.Records,
		References:         release.References,
		PreviewFingerprint: release.PreviewFingerprint,
	}
	state.Documents = release.Documents
	state.ReleaseID = entry.ID
	state.History = append([]StoredGroupRelease{entry}, state.History...)
	return s.writeState(state)
}

// GroupRollback describes what a group rollback restored and what it could
// not restore.
type GroupRollback struct {
	ReleaseID string
	Documents Group
	// Restored lists the members returned to the historical snapshot.
	Restored []string
	// Unrecoverable lists information the snapshot does not contain, such as
	// fields dropped by the original migration. Their old values survive only
	// inside the stored release originals; they are not part of the restored
	// group.
	Unrecoverable []string
}

// Rollback replaces the current group with the documents of a historical
// release. The release itself and all originals stay in the history, so no
// previously published state is destroyed by rolling back.
func (s *GroupStore) Rollback(releaseID string) (GroupRollback, error) {
	state, err := s.readState()
	if err != nil {
		return GroupRollback{}, err
	}
	var target *StoredGroupRelease
	for i := range state.History {
		if state.History[i].ID == releaseID {
			target = &state.History[i]
			break
		}
	}
	if target == nil {
		return GroupRollback{}, fmt.Errorf("%w: release %q", ErrReleaseNotFound, releaseID)
	}

	rollback := GroupRollback{ReleaseID: releaseID, Documents: Group(target.Documents).Clone()}
	for _, name := range sortedNames(Group(target.Documents)) {
		rollback.Restored = append(rollback.Restored, name)
	}
	for name, original := range target.Originals {
		current := target.Documents[name]
		for field := range original {
			if field == "version" {
				continue
			}
			if _, kept := current[field]; !kept {
				rollback.Unrecoverable = append(rollback.Unrecoverable, fmt.Sprintf("member %q: field %q was dropped by the migration and is only preserved in the release originals, not in the restored group", name, field))
			}
		}
	}

	state.Documents = target.Documents
	state.ReleaseID = target.ID
	if err := s.writeState(state); err != nil {
		return GroupRollback{}, err
	}
	return rollback, nil
}
