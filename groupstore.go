package configmigrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// GroupStore publishes whole groups atomically into a directory. Every
// generation lives in its own subdirectory; a single manifest rename selects
// the current generation, so after an interrupted save or publish a reopened
// store exposes either the complete old group or the complete new group,
// never a mixture. Original files are never deleted.
type GroupStore struct {
	dir string
}

// NewGroupStore creates a store rooted at dir.
func NewGroupStore(dir string) *GroupStore {
	return &GroupStore{dir: dir}
}

type groupManifest struct {
	Current  string               `json:"current"`
	Releases []StoredGroupRelease `json:"history"`
	Previews []StoredGroupPreview `json:"previews"`
}

// StoredGroupRelease preserves one complete group transaction.
type StoredGroupRelease struct {
	Generation string              `json:"generation"`
	Timestamp  time.Time           `json:"timestamp"`
	Originals  Group               `json:"originals"`
	Documents  Group               `json:"documents"`
	Records    map[string][]Record `json:"records"`
}

// StoredGroupPreview keeps a confirmed preview together with the fingerprint
// it was based on, so historical previews remain queryable with their basis.
type StoredGroupPreview struct {
	Timestamp time.Time    `json:"timestamp"`
	Preview   GroupPreview `json:"preview"`
}

// GroupState is the currently visible complete group.
type GroupState struct {
	Generation string
	Documents  Group
	History    []StoredGroupRelease
	Previews   []StoredGroupPreview
}

func (s *GroupStore) manifestPath() string { return filepath.Join(s.dir, "manifest.json") }

func (s *GroupStore) readManifest() (groupManifest, error) {
	contents, err := os.ReadFile(s.manifestPath())
	if errors.Is(err, os.ErrNotExist) {
		return groupManifest{}, ErrNotFound
	}
	if err != nil {
		return groupManifest{}, err
	}
	var manifest groupManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return groupManifest{}, fmt.Errorf("decode group manifest: %w", err)
	}
	return manifest, nil
}

// Load returns the complete current group. Before the first successful
// publish it returns ErrNotFound; a half-written generation is never visible.
func (s *GroupStore) Load() (GroupState, error) {
	manifest, err := s.readManifest()
	if err != nil {
		return GroupState{}, err
	}
	for _, release := range manifest.Releases {
		if release.Generation == manifest.Current {
			return GroupState{
				Generation: release.Generation,
				Documents:  release.Documents.Clone(),
				History:    manifest.Releases,
				Previews:   manifest.Previews,
			}, nil
		}
	}
	return GroupState{}, fmt.Errorf("manifest points at unknown generation %q", manifest.Current)
}

// History returns every stored release and preview without loading documents.
func (s *GroupStore) History() ([]StoredGroupRelease, []StoredGroupPreview, error) {
	manifest, err := s.readManifest()
	if err != nil {
		return nil, nil, err
	}
	return manifest.Releases, manifest.Previews, nil
}

// Publish returns a callback suitable for Engine.ApplyGroup.
func (s *GroupStore) Publish() func(context.Context, GroupRelease) error {
	return func(_ context.Context, release GroupRelease) error {
		return s.Commit(release)
	}
}

// Commit writes the new generation completely, then flips the manifest. A
// crash before the rename leaves the old generation current; a crash after
// leaves the new one complete. Orphan generation directories are ignored.
func (s *GroupStore) Commit(release GroupRelease) error {
	manifest, err := s.readManifest()
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	generation := fmt.Sprintf("g%06d", len(manifest.Releases)+1)
	genDir := filepath.Join(s.dir, "generations", generation)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return err
	}
	for _, name := range sortedNames(release.Documents) {
		contents, err := json.MarshalIndent(release.Documents[name], "", "  ")
		if err != nil {
			return err
		}
		if err := writeFileSync(filepath.Join(genDir, name+".json"), append(contents, '\n')); err != nil {
			return err
		}
	}
	manifest.Releases = append(manifest.Releases, StoredGroupRelease{
		Generation: generation,
		Timestamp:  timeNow().UTC(),
		Originals:  release.Originals.Clone(),
		Documents:  release.Documents.Clone(),
		Records:    release.Records,
	})
	if release.Preview.Fingerprint != "" {
		manifest.Previews = append(manifest.Previews, StoredGroupPreview{
			Timestamp: timeNow().UTC(),
			Preview:   release.Preview,
		})
	}
	manifest.Current = generation
	return s.writeManifest(manifest)
}

// RecordPreview stores a preview that was computed but not (yet) published,
// so its basis remains queryable later.
func (s *GroupStore) RecordPreview(preview GroupPreview) error {
	manifest, err := s.readManifest()
	if errors.Is(err, ErrNotFound) {
		manifest = groupManifest{}
	} else if err != nil {
		return err
	}
	manifest.Previews = append(manifest.Previews, StoredGroupPreview{
		Timestamp: timeNow().UTC(),
		Preview:   preview,
	})
	return s.writeManifest(manifest)
}

// RollbackReport describes what switching back to a generation restores and
// what cannot be recovered.
type RollbackReport struct {
	Generation string   `json:"generation"`
	Restored   []string `json:"restored"`
	Warnings   []string `json:"warnings,omitempty"`
}

// Rollback switches the current generation back to a previously published
// one. Original generation directories are kept, so nothing is destroyed;
// the report states exactly which files are restored and which changes made
// after that generation become unreachable from the current pointer.
func (s *GroupStore) Rollback(generation string) (RollbackReport, error) {
	manifest, err := s.readManifest()
	if err != nil {
		return RollbackReport{}, err
	}
	var target *StoredGroupRelease
	for i := range manifest.Releases {
		if manifest.Releases[i].Generation == generation {
			target = &manifest.Releases[i]
		}
	}
	if target == nil {
		return RollbackReport{}, fmt.Errorf("%w: generation %q", ErrNotFound, generation)
	}
	report := RollbackReport{Generation: generation}
	for _, name := range sortedNames(target.Documents) {
		report.Restored = append(report.Restored, name)
	}
	if manifest.Current != generation {
		for _, release := range manifest.Releases {
			if release.Generation > generation && release.Generation <= manifest.Current {
				report.Warnings = append(report.Warnings, fmt.Sprintf("generation %s remains archived but is no longer current; edits made only there are not restored", release.Generation))
			}
		}
	}
	sort.Strings(report.Warnings)
	manifest.Current = generation
	if err := s.writeManifest(manifest); err != nil {
		return RollbackReport{}, err
	}
	return report, nil
}

func (s *GroupStore) writeManifest(manifest groupManifest) error {
	contents, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.dir, ".manifest-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
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
	return os.Rename(temp.Name(), s.manifestPath())
}

func writeFileSync(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
