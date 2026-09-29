package configmigrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileStore migrates JSON configuration files. Every successful migration
// retains the original file and writes an append-only migration record.
type FileStore struct {
	migrator *Migrator
	clock    func() time.Time
}

// NewFileStore creates a JSON file store backed by a Migrator.
func NewFileStore(migrator *Migrator) (*FileStore, error) {
	if migrator == nil {
		return nil, errors.New("migrator must not be nil")
	}
	return &FileStore{migrator: migrator, clock: time.Now}, nil
}

// MigrationFile is an auditable JSON migration record.
type MigrationFile struct {
	Path          string       `json:"path"`
	SourceVersion string       `json:"source_version"`
	TargetVersion string       `json:"target_version"`
	Changed       bool         `json:"changed"`
	MigratedAt    time.Time    `json:"migrated_at"`
	Steps         []StepRecord `json:"steps"`
}

// MigrateFile migrates path to target. It validates the complete path and runs
// all steps against an in-memory copy before replacing the file. On success,
// the previous content is copied to backupDir and a JSON record is written to
// recordDir. Existing destination files are never overwritten.
func (s *FileStore) MigrateFile(path, target, backupDir, recordDir string, opts *PlanOptions) (Result, error) {
	input, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	var config Config
	if err := json.Unmarshal(input, &config); err != nil {
		return Result{}, fmt.Errorf("decode %s: %w", path, err)
	}

	result, err := s.migrator.Migrate(config, target, opts)
	if err != nil {
		return Result{}, err
	}
	if !result.Changed {
		return result, nil
	}

	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return Result{}, err
	}
	encoded = append(encoded, '\n')
	info, err := os.Stat(path)
	if err != nil {
		return Result{}, err
	}

	cleanup, stagedConfig, err := stageFile(path, encoded, info.Mode())
	if err != nil {
		return Result{}, err
	}
	defer cleanup()

	migration := MigrationFile{
		Path:          path,
		SourceVersion: result.SourceVersion,
		TargetVersion: result.TargetVersion,
		Changed:       result.Changed,
		MigratedAt:    s.clock().UTC(),
		Steps:         result.Steps,
	}
	recordEncoded, err := json.MarshalIndent(migration, "", "  ")
	if err != nil {
		return Result{}, err
	}
	recordEncoded = append(recordEncoded, '\n')

	backupFinal, backupStaged, backupCleanup, err := reserveUnique(backupDir, uniqueName(filepath.Base(path), migration.MigratedAt, "bak"))
	if err != nil {
		return Result{}, err
	}
	defer backupCleanup()
	if err := os.WriteFile(backupStaged, input, info.Mode()); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(backupStaged, info.Mode().Perm()); err != nil {
		return Result{}, err
	}

	recordFinal, recordStaged, recordCleanup, err := reserveUnique(recordDir, uniqueName(filepath.Base(path), migration.MigratedAt, "migration.json"))
	if err != nil {
		return Result{}, err
	}
	defer recordCleanup()
	if err := os.WriteFile(recordStaged, recordEncoded, 0o644); err != nil {
		return Result{}, err
	}

	if err := os.Rename(backupStaged, backupFinal); err != nil {
		return Result{}, err
	}
	if err := os.Rename(recordStaged, recordFinal); err != nil {
		_ = os.Remove(backupFinal)
		return Result{}, err
	}
	if err := os.Rename(stagedConfig, path); err != nil {
		_ = os.Remove(recordFinal)
		_ = os.Remove(backupFinal)
		return Result{}, err
	}
	cleanup = func() {}
	backupCleanup = func() {}
	recordCleanup = func() {}
	return result, nil
}

func stageFile(path string, content []byte, mode os.FileMode) (func(), string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, "", err
	}
	tempPath := file.Name()
	cleanup := func() { _ = os.Remove(tempPath) }
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		cleanup()
		return nil, "", err
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		_ = file.Close()
		cleanup()
		return nil, "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanup()
		return nil, "", err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, "", err
	}
	return cleanup, tempPath, nil
}

func reserveUnique(dir, base string) (finalPath, stagedPath string, cleanup func(), err error) {
	if dir == "" {
		return "", "", nil, errors.New("directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", nil, err
	}
	candidate := filepath.Join(dir, base)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 0; ; i++ {
		stagedName := fmt.Sprintf(".%s.staged-%d%s", stem, i, ext)
		if i == 0 {
			stagedName = fmt.Sprintf(".%s.staged%s", stem, ext)
		}
		staged := filepath.Join(dir, stagedName)
		stagedFile, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", "", nil, err
		}
		_ = stagedFile.Close()

		if i > 0 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s.%d%s", stem, i, ext))
		}
		if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(staged)
			if err != nil {
				return "", "", nil, err
			}
			continue
		}
		cleanup := func() {
			_ = os.Remove(staged)
		}
		return candidate, staged, cleanup, nil
	}
}

func uniqueName(base string, at time.Time, suffix string) string {
	baseExt := filepath.Ext(base)
	stem := strings.TrimSuffix(base, baseExt)
	return fmt.Sprintf("%s.%s.%s", stem, at.Format("20060102T150405.000000000Z0700"), suffix)
}

// Routes returns all available paths between two versions. It is useful for
// presenting branch choices to users before migration.
func (m *Migrator) Routes(from, to string) ([][]string, error) {
	steps, ok := m.findRoutes(from, to)
	if !ok {
		return nil, fmt.Errorf("no migration path from %q to %q", from, to)
	}
	routes := make([][]string, 0, len(steps))
	for _, route := range steps {
		versions := []string{from}
		for _, step := range route {
			versions = append(versions, step.ToVersion)
		}
		routes = append(routes, versions)
	}
	sort.Slice(routes, func(i, j int) bool {
		return strings.Join(routes[i], ",") < strings.Join(routes[j], ",")
	})
	return routes, nil
}
