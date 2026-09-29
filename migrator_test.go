package configmigrator

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMigrateSplitsFieldAddsDefaultAndRecordsSteps(t *testing.T) {
	m, err := New(
		Step{
			FromVersion: "v1",
			ToVersion:   "v2",
			Migrate: func(config Config) error {
				config["version"] = "v2"
				config["retries"] = 3
				return nil
			},
		},
		Step{
			FromVersion: "v2",
			ToVersion:   "v3",
			Migrate: func(config Config) error {
				name, ok := config["name"].(string)
				if !ok {
					return errors.New("name must be a string")
				}
				parts := strings.SplitN(name, " ", 2)
				config["first_name"] = parts[0]
				if len(parts) == 2 {
					config["last_name"] = parts[1]
				}
				delete(config, "name")
				config["version"] = "v3"
				return nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	config := Config{"version": "v1", "name": "Ada Lovelace"}
	original := cloneConfig(config)
	result, err := m.Migrate(config, "v3", nil)
	if err != nil {
		t.Fatal(err)
	}

	want := Config{
		"version":    "v3",
		"retries":    3,
		"first_name": "Ada",
		"last_name":  "Lovelace",
	}
	if !reflect.DeepEqual(config, want) {
		t.Fatalf("config = %#v, want %#v", config, want)
	}
	if !result.Changed || result.SourceVersion != "v1" || result.TargetVersion != "v3" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(result.Steps) != 2 || result.Steps[1].FromVersion != "v2" {
		t.Fatalf("unexpected step records: %#v", result.Steps)
	}
	if reflect.DeepEqual(original, config) {
		t.Fatal("test setup did not produce a changed configuration")
	}
}

func TestMigrationFailureRestoresOriginal(t *testing.T) {
	m, _ := New(
		Step{FromVersion: "v1", ToVersion: "v2", Migrate: func(config Config) error {
			config["version"] = "v2"
			config["items"] = []any{"kept"}
			return nil
		}},
		Step{FromVersion: "v2", ToVersion: "v3", Migrate: func(config Config) error {
			config["version"] = "v3-bad"
			config["items"].([]any)[0] = "changed"
			return errors.New("boom")
		}},
	)
	config := Config{"version": "v1", "items": []any{"kept"}}
	original := cloneConfig(config)

	_, err := m.Migrate(config, "v3", nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want boom", err)
	}
	if !reflect.DeepEqual(config, original) {
		t.Fatalf("config changed after failure: %#v", config)
	}
}

func TestFailureRestoresStronglyTypedNestedValues(t *testing.T) {
	m, _ := New(Step{FromVersion: "v1", ToVersion: "v2", Migrate: func(config Config) error {
		items := config["items"].([]string)
		items[0] = "changed"
		config["nested"].(map[string]int)["count"] = 99
		return errors.New("reject after mutation")
	}})
	config := Config{
		"version": "v1",
		"items":   []string{"original"},
		"nested":  map[string]int{"count": 1},
	}
	original := cloneConfig(config)

	_, err := m.Migrate(config, "v2", nil)
	if err == nil || !strings.Contains(err.Error(), "reject after mutation") {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(config, original) {
		t.Fatalf("config changed after failure: %#v", config)
	}
}

func TestVersionMismatchRestoresOriginal(t *testing.T) {
	m, _ := New(Step{FromVersion: "v1", ToVersion: "v2", Migrate: func(config Config) error {
		config["version"] = "other"
		return nil
	}})
	config := Config{"version": "v1", "value": "original"}
	original := cloneConfig(config)

	_, err := m.Migrate(config, "v2", nil)
	if err == nil || !strings.Contains(err.Error(), "reported version") {
		t.Fatalf("error = %v, want version mismatch", err)
	}
	if !reflect.DeepEqual(config, original) {
		t.Fatalf("config = %#v, want original %#v", config, original)
	}
}

func TestAlreadyAtTargetIsIdempotent(t *testing.T) {
	called := false
	m, _ := New(Step{FromVersion: "v1", ToVersion: "v2", Migrate: func(config Config) error {
		called = true
		return nil
	}})
	config := Config{"version": "v2", "value": "same"}

	result, err := m.Migrate(config, "v2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || called {
		t.Fatalf("target migration changed state: result=%#v called=%v", result, called)
	}
}

func TestPlanBranchesRequireExplicitRoute(t *testing.T) {
	m, _ := New(
		Step{Name: "stable", FromVersion: "v1", ToVersion: "v2", Migrate: func(Config) error { return nil }},
		Step{Name: "experimental", FromVersion: "v1", ToVersion: "v2x", Migrate: func(Config) error { return nil }},
		Step{FromVersion: "v2", ToVersion: "v3", Migrate: func(Config) error { return nil }},
		Step{FromVersion: "v2x", ToVersion: "v3", Migrate: func(Config) error { return nil }},
	)

	if _, err := m.Plan("v1", "v3", nil); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want ambiguous", err)
	}
	route, err := m.Plan("v1", "v3", &PlanOptions{Route: []string{"v2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(route) != 2 || route[0].ToVersion != "v2" {
		t.Fatalf("route = %#v", route)
	}
	if _, err := m.Plan("v1", "v3", &PlanOptions{Route: []string{"missing"}}); err == nil {
		t.Fatal("unknown route was accepted")
	}
}

func TestMissingAndReverseMigrationsAreRejected(t *testing.T) {
	m, _ := New(Step{FromVersion: "v1", ToVersion: "v2", Migrate: func(Config) error { return nil }})

	_, err := m.Plan("v2", "v1", nil)
	if err == nil || !strings.Contains(err.Error(), "no migration path") {
		t.Fatalf("reverse error = %v", err)
	}
	_, err = m.Plan("v2", "v9", nil)
	if err == nil || !strings.Contains(err.Error(), "no migration path") {
		t.Fatalf("missing error = %v", err)
	}
}

func TestFileMigrationIsAtomicAndAuditable(t *testing.T) {
	m, _ := New(Step{FromVersion: "v1", ToVersion: "v2", Migrate: func(config Config) error {
		config["version"] = "v2"
		config["enabled"] = true
		return nil
	}})
	store, err := NewFileStore(m)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 9, 29, 10, 11, 12, 34, time.UTC)
	call := 0
	store.clock = func() time.Time {
		call++
		if call == 1 {
			return fixed
		}
		return fixed.Add(time.Millisecond)
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "app.json")
	original := []byte("{\n  \"version\": \"v1\"\n}\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dir, "backups")
	recordDir := filepath.Join(dir, "records")

	result, err := store.MigrateFile(configPath, "v2", backupDir, recordDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("migration was not marked changed")
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var migrated Config
	if err := json.Unmarshal(content, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated["version"] != "v2" || migrated["enabled"] != true {
		t.Fatalf("unexpected migrated config: %s", content)
	}

	backup := filepath.Join(backupDir, "app.20260929T101112.000000034Z.bak")
	if got, err := os.ReadFile(backup); err != nil || !reflect.DeepEqual(got, original) {
		t.Fatalf("backup = %q, %v; want original", got, err)
	}
	record := filepath.Join(recordDir, "app.20260929T101112.000000034Z.migration.json")
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var migration MigrationFile
	if err := json.Unmarshal(data, &migration); err != nil {
		t.Fatal(err)
	}
	if migration.SourceVersion != "v1" || migration.TargetVersion != "v2" || len(migration.Steps) != 1 {
		t.Fatalf("unexpected migration record: %#v", migration)
	}

	before, _ := os.ReadFile(configPath)
	result, err = store.MigrateFile(configPath, "v2", backupDir, recordDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(configPath)
	if result.Changed || !reflect.DeepEqual(before, after) {
		t.Fatalf("repeat migration changed content or result: %#v", result)
	}
}

func TestRepeatedMigrationCreatesUniqueArtifacts(t *testing.T) {
	m, _ := New(
		Step{FromVersion: "v1", ToVersion: "v2", Migrate: func(config Config) error {
			config["version"] = "v2"
			return nil
		}},
		Step{FromVersion: "v2", ToVersion: "v3", Migrate: func(config Config) error {
			config["version"] = "v3"
			return nil
		}},
	)
	store, _ := NewFileStore(m)
	store.clock = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC) }
	dir := t.TempDir()
	path := filepath.Join(dir, "app.json")
	if err := os.WriteFile(path, []byte("{\"version\":\"v1\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dir, "b")
	recordDir := filepath.Join(dir, "r")
	if _, err := store.MigrateFile(path, "v2", backupDir, recordDir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MigrateFile(path, "v3", backupDir, recordDir, nil); err != nil {
		t.Fatal(err)
	}
	backups, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := os.ReadDir(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 2 || len(records) != 2 {
		t.Fatalf("got %d backups and %d records, want 2 each", len(backups), len(records))
	}
}

func TestFileMigrationFailureLeavesOriginalAndNoPublishedArtifacts(t *testing.T) {
	m, _ := New(Step{FromVersion: "v1", ToVersion: "v2", Migrate: func(config Config) error {
		config["version"] = "v2"
		return errors.New("rejected")
	}})
	store, _ := NewFileStore(m)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "app.json")
	original := []byte("{\"version\":\"v1\"}\n")
	if err := os.WriteFile(configPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := store.MigrateFile(configPath, "v2", filepath.Join(dir, "b"), filepath.Join(dir, "r"), nil)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("error = %v", err)
	}
	if got, _ := os.ReadFile(configPath); !reflect.DeepEqual(got, original) {
		t.Fatalf("config = %q, want %q", got, original)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "b"))
	if len(entries) != 0 {
		t.Fatalf("backup artifacts leaked: %#v", entries)
	}
	entries, _ = os.ReadDir(filepath.Join(dir, "r"))
	if len(entries) != 0 {
		t.Fatalf("record artifacts leaked: %#v", entries)
	}
}
