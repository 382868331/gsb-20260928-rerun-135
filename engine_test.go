package configmigrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func mustDoc(version string, values map[string]any) Document {
	doc := Document{"version": version}
	for key, value := range values {
		doc[key] = value
	}
	return doc
}

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	engine := NewEngine()
	engine.MustRegister(Step{
		From:        "v1",
		To:          "v2",
		Description: "add default timeout",
		MigrateDoc: func(_ context.Context, doc Document) (StepResult, error) {
			next := doc.Clone()
			next["version"] = "v2"
			if _, ok := next["timeout_seconds"]; !ok {
				next["timeout_seconds"] = 30
			}
			return StepResult{Document: next, Changes: []FieldChange{{Field: "timeout_seconds", Action: ActionAdd, After: 30, Sources: []FieldSource{{Kind: SourceDefault, Value: 30}}}}}, nil
		},
	})
	engine.MustRegister(Step{
		From:        "v2",
		To:          "v3",
		Description: "split endpoint",
		MigrateDoc: func(_ context.Context, doc Document) (StepResult, error) {
			endpoint, ok := doc["endpoint"].(string)
			if !ok {
				return StepResult{}, errors.New("endpoint is required")
			}
			if _, conflict := doc["host"]; conflict {
				return StepResult{}, errors.New("host already exists and conflicts with endpoint split")
			}
			next := doc.Clone()
			next["version"] = "v3"
			next["host"] = endpoint
			next["port"] = 443
			delete(next, "endpoint")
			return StepResult{Document: next, Changes: []FieldChange{
				{Field: "host", Action: ActionAdd, After: endpoint, Sources: []FieldSource{{Kind: SourceSplit, Field: "endpoint", Value: endpoint, Rule: "endpoint hostname"}}},
				{Field: "port", Action: ActionAdd, After: 443, Sources: []FieldSource{{Kind: SourceDefault, Value: 443}}},
				{Field: "endpoint", Action: ActionRemove, Before: endpoint, Sources: []FieldSource{{Kind: SourceSplit, Field: "endpoint", Value: endpoint, Rule: "endpoint hostname"}}},
			}}, nil
		},
		ReverseDoc: func(_ context.Context, doc Document) (StepResult, error) {
			host, ok := doc["host"].(string)
			if !ok {
				return StepResult{}, errors.New("host is required")
			}
			next := doc.Clone()
			next["version"] = "v2"
			next["endpoint"] = host
			delete(next, "host")
			delete(next, "port")
			return StepResult{Document: next, Changes: []FieldChange{
				{Field: "endpoint", Action: ActionAdd, After: host, Sources: []FieldSource{{Kind: SourceOldValue, Field: "host", Value: host}}},
				{Field: "host", Action: ActionRemove, Before: host, Sources: []FieldSource{{Kind: SourceOldValue, Field: "host", Value: host}}},
				{Field: "port", Action: ActionRemove, Before: doc["port"], Loss: true, Description: "combined endpoint cannot retain an explicit port"},
			}}, nil
		},
	})
	engine.MustRegister(Step{
		From:        "v2",
		To:          "v3-alt",
		Description: "alternative endpoint split",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			next := doc.Clone()
			next["version"] = "v3-alt"
			next["service_url"] = doc["endpoint"]
			return next, nil
		},
	})
	engine.MustRegister(Step{
		From: "v3-alt",
		To:   "v4",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			next := doc.Clone()
			next["version"] = "v4"
			return next, nil
		},
	})
	engine.MustRegister(Step{
		From: "v3",
		To:   "v4",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			if failed, _ := doc["fail_v4"].(bool); failed {
				return nil, errors.New("simulated v4 failure")
			}
			next := doc.Clone()
			next["version"] = "v4"
			return next, nil
		},
	})
	return engine
}

func TestMigrateAppliesDefaultsAndSplit(t *testing.T) {
	engine := newTestEngine(t)
	original := mustDoc("v1", map[string]any{"endpoint": "api.example.com"})
	before := original.Clone()

	result, err := engine.Migrate(context.Background(), original, "v3")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated || result.ToVersion != "v3" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Document["timeout_seconds"] != 30 {
		t.Fatalf("default was not applied: %#v", result.Document)
	}
	if result.Document["host"] != "api.example.com" || result.Document["port"] != 443 {
		t.Fatalf("endpoint was not split: %#v", result.Document)
	}
	if _, exists := result.Document["endpoint"]; exists {
		t.Fatal("old endpoint field was retained")
	}
	if fmt.Sprint(original) != fmt.Sprint(before) {
		t.Fatalf("input mutated: %#v", original)
	}
	if len(result.Records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(result.Records))
	}
}

func TestAlreadyAtTargetIsIdempotent(t *testing.T) {
	engine := newTestEngine(t)
	original := mustDoc("v3", map[string]any{"host": "api.example.com", "port": float64(443)})

	result, err := engine.Migrate(context.Background(), original, "v3")
	if err != nil {
		t.Fatal(err)
	}
	if result.Migrated || len(result.Records) != 0 {
		t.Fatalf("repeat migration changed content: %+v", result)
	}
	if fmt.Sprint(result.Document) != fmt.Sprint(original) {
		t.Fatalf("document changed: %#v", result.Document)
	}
}

func TestMissingStepAndBranchAreDetectedBeforePublish(t *testing.T) {
	engine := newTestEngine(t)
	doc := mustDoc("v1", nil)
	published := false

	_, err := engine.Publish(context.Background(), doc, "v9", func(context.Context, Release) error {
		published = true
		return nil
	})
	if !errors.Is(err, ErrNoMigrationPath) || published {
		t.Fatalf("missing path should prevent publish, err=%v published=%v", err, published)
	}

	_, err = engine.Plan("v2", "v4")
	if !errors.Is(err, ErrAmbiguousPath) {
		t.Fatalf("expected ambiguous path, got %v", err)
	}

	steps, err := engine.Plan("v2", "v4", WithPath("v2", "v3", "v4"))
	if err != nil || len(steps) != 2 {
		t.Fatalf("explicit path failed: steps=%v err=%v", steps, err)
	}
}

func TestConflictAndMidMigrationFailureDoNotPublish(t *testing.T) {
	engine := newTestEngine(t)
	doc := mustDoc("v3", map[string]any{"host": "old", "fail_v4": true})
	published := false

	_, err := engine.Publish(context.Background(), doc, "v4", func(context.Context, Release) error {
		published = true
		return nil
	})
	if err == nil || published {
		t.Fatalf("failure must not publish, err=%v published=%v", err, published)
	}

	conflict := mustDoc("v2", map[string]any{
		"endpoint": "api.example.com",
		"host":     "existing.example.com",
	})
	_, err = engine.Migrate(context.Background(), conflict, "v3")
	if err == nil {
		t.Fatal("expected field conflict error")
	}
}

func TestReverse(t *testing.T) {
	engine := newTestEngine(t)
	doc := mustDoc("v3", map[string]any{"host": "api.example.com", "port": 443})

	result, err := engine.Reverse(context.Background(), doc, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if result.Document["endpoint"] != "api.example.com" {
		t.Fatalf("reverse did not restore endpoint: %#v", result.Document)
	}

	_, err = engine.Reverse(context.Background(), doc, "v1")
	if !errors.Is(err, ErrReverseUnsupported) {
		t.Fatalf("expected unsupported reverse, got %v", err)
	}
}

func findChange(t *testing.T, changes []FieldChange, field string) FieldChange {
	t.Helper()
	for _, change := range changes {
		if change.Field == field {
			return change
		}
	}
	t.Fatalf("change for %q was not reported in %#v", field, changes)
	return FieldChange{}
}

func TestPreviewExplainsSourcesAndDoesNotClaimLosslessSplit(t *testing.T) {
	engine := newTestEngine(t)
	doc := mustDoc("v1", map[string]any{"endpoint": "api.example.com"})

	preview, err := engine.Preview(context.Background(), doc, "v3")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Result.Lossy {
		t.Fatal("documented split removal should not be treated as unexplained information loss")
	}
	host := findChange(t, preview.Result.Changes, "host")
	if host.Action != ActionAdd || host.Sources[0].Kind != SourceSplit || host.Sources[0].Field != "endpoint" {
		t.Fatalf("host provenance was not reported: %#v", host)
	}
	timeout := findChange(t, preview.Result.Changes, "timeout_seconds")
	if timeout.Sources[0].Kind != SourceDefault {
		t.Fatalf("timeout provenance was not reported as default: %#v", timeout)
	}

	reverse, err := engine.PreviewReverse(context.Background(), preview.Result.Document, "v2")
	if err != nil {
		t.Fatal(err)
	}
	port := findChange(t, reverse.Result.Changes, "port")
	if !port.Loss {
		t.Fatalf("reverse port removal must be marked as lossy: %#v", port)
	}
	if !reverse.Result.Lossy {
		t.Fatal("reverse preview must not advertise a lossless restore")
	}

	changedReverseInput := preview.Result.Document.Clone()
	changedReverseInput["host"] = "changed.example.com"
	if _, err := engine.ApplyReversePreview(context.Background(), changedReverseInput, reverse); !errors.Is(err, ErrPreviewStale) {
		t.Fatalf("changed rollback input must reject its preview, got %v", err)
	}

	rollback, err := engine.ApplyReversePreview(context.Background(), preview.Result.Document, reverse)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Document["version"] != "v2" || rollback.Document["endpoint"] != "api.example.com" {
		t.Fatalf("rollback preview was not applied: %#v", rollback.Document)
	}
}

func TestApplyPreviewRejectsChangedInputAndUsesSelectedPath(t *testing.T) {
	engine := newTestEngine(t)
	original := mustDoc("v2", map[string]any{"endpoint": "api.example.com"})
	preview, err := engine.Preview(context.Background(), original, "v4", WithPath("v2", "v3", "v4"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(preview.Path) != "[v2 v3 v4]" {
		t.Fatalf("unexpected selected path: %v", preview.Path)
	}

	changed := original.Clone()
	changed["endpoint"] = "changed.example.com"
	published := false
	_, err = engine.PublishPreview(context.Background(), changed, preview, func(context.Context, Release) error {
		published = true
		return nil
	})
	if !errors.Is(err, ErrPreviewStale) || published {
		t.Fatalf("changed input must reject the old preview, err=%v published=%v", err, published)
	}

	result, err := engine.ApplyPreview(context.Background(), original, preview)
	if err != nil {
		t.Fatal(err)
	}
	if result.Document["version"] != "v4" || result.Document["host"] != "api.example.com" {
		t.Fatalf("preview was not applied: %#v", result.Document)
	}
}

func TestReverseUnsupportedReportsSpecificBlocker(t *testing.T) {
	engine := newTestEngine(t)
	doc := mustDoc("v3", map[string]any{"host": "api.example.com", "port": 443})

	_, err := engine.Reverse(context.Background(), doc, "v1")
	if !errors.Is(err, ErrReverseUnsupported) {
		t.Fatalf("expected unsupported reverse, got %v", err)
	}
	var blockers ReverseBlockers
	if !errors.As(err, &blockers) || len(blockers) != 1 {
		t.Fatalf("expected one structured blocker, got %v", err)
	}
	if blockers[0].From != "v2" || blockers[0].To != "v1" {
		t.Fatalf("blocker did not identify the missing edge: %#v", blockers[0])
	}

	partial, partialErr := engine.Reverse(context.Background(), doc, "v1")
	if partialErr == nil || partial.Document != nil {
		t.Fatalf("unsupported rollback must not return a partial document: result=%#v err=%v", partial, partialErr)
	}
}

func TestJSONStoreAtomicHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "config.json")
	store := NewJSONStore(path)
	engine := newTestEngine(t)

	if _, _, err := store.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}

	original := mustDoc("v1", map[string]any{"endpoint": "api.example.com"})
	if _, err := engine.Publish(context.Background(), original, "v3", store.Publish()); err != nil {
		t.Fatal(err)
	}
	current, history, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if current["version"] != "v3" || len(history) != 1 {
		t.Fatalf("unexpected state: %#v history=%d", current, len(history))
	}
	if history[0].Original["version"] != "v1" {
		t.Fatalf("original document was not retained: %#v", history[0].Original)
	}
	if len(history[0].Path) != 2 {
		t.Fatalf("migration record incomplete: %#v", history[0].Path)
	}
	if fmt.Sprint(history[0].PathVersions) != "[v1 v2 v3]" {
		t.Fatalf("actual path versions were not retained: %v", history[0].PathVersions)
	}
	host := findChange(t, history[0].Result.Changes, "host")
	if host.Sources[0].Kind != SourceSplit {
		t.Fatalf("published result lost field provenance: %#v", host)
	}

	failed := mustDoc("v3", map[string]any{"host": "api.example.com", "fail_v4": true})
	if _, err := engine.Publish(context.Background(), failed, "v4", store.Publish()); err == nil {
		t.Fatal("expected failed migration")
	}
	current, history, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if current["version"] != "v3" || len(history) != 1 {
		t.Fatalf("failed release changed published state: version=%v history=%d", current["version"], len(history))
	}
}

func TestJSONStorePublisherFailureLeavesPriorRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewJSONStore(path)
	engine := newTestEngine(t)
	original := mustDoc("v3", map[string]any{"host": "api.example.com"})
	if _, err := engine.Publish(context.Background(), original, "v4", store.Publish()); err != nil {
		t.Fatal(err)
	}

	doc := mustDoc("v3", map[string]any{"host": "second.example.com"})
	_, err := engine.Publish(context.Background(), doc, "v4", func(context.Context, Release) error {
		return errors.New("publisher unavailable")
	})
	if err == nil {
		t.Fatal("expected publisher error")
	}
	current, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if current["version"] != "v4" {
		t.Fatalf("prior release was damaged: %#v", current)
	}
	if leftover, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-state-*.tmp")); err != nil || len(leftover) != 0 {
		t.Fatalf("temporary files left behind: %v, %v", leftover, err)
	}
}

func TestInvalidRegistration(t *testing.T) {
	engine := NewEngine()
	if err := engine.Register(Step{From: "v1", To: "v1", Migrate: func(context.Context, Document) (Document, error) { return nil, nil }}); !errors.Is(err, ErrInvalidStep) {
		t.Fatalf("expected invalid step, got %v", err)
	}
}
