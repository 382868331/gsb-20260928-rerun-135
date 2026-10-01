package configmigrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newGroupEngine(t *testing.T) *Engine {
	t.Helper()
	engine := NewEngine()
	engine.MustRegister(Step{
		From: "v1", To: "v2", Description: "split endpoint into host and advance",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			out := doc.Clone()
			out["version"] = "v2"
			if endpoint, ok := out["endpoint"]; ok {
				out["host"] = endpoint
				delete(out, "endpoint")
			}
			return out, nil
		},
		Reverse: func(_ context.Context, doc Document) (Document, error) {
			out := doc.Clone()
			out["version"] = "v1"
			if host, ok := out["host"]; ok {
				out["endpoint"] = host
				delete(out, "host")
			}
			return out, nil
		},
	})
	engine.MustRegister(Step{
		From: "v2", To: "v3", Description: "rename identity",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			out := doc.Clone()
			out["version"] = "v3"
			if host, ok := out["host"].(string); ok {
				out["host"] = "new-" + host
			}
			if upstream, ok := out["upstream"].(string); ok {
				out["upstream"] = "new-" + upstream
			}
			return out, nil
		},
	})
	return engine
}

func groupFixture() (Group, map[string]string, []Reference) {
	group := Group{
		"app": mustDoc("v1", map[string]any{"endpoint": "api.internal"}),
		"lb":  mustDoc("v1", map[string]any{"backend_host": "api.internal"}),
	}
	targets := map[string]string{"app": "v2", "lb": "v2"}
	refs := []Reference{{Name: "backend", From: FileField{"app", "host"}, To: FileField{"lb", "backend_host"}}}
	return group, targets, refs
}

func TestPreviewGroupShowsReferencesFollowingSplit(t *testing.T) {
	engine := newGroupEngine(t)
	group, targets, refs := groupFixture()
	preview, err := engine.PreviewGroup(context.Background(), group, targets, refs)
	if err != nil {
		t.Fatalf("PreviewGroup: %v", err)
	}
	if len(preview.Blockers) != 0 {
		t.Fatalf("unexpected blockers: %v", preview.Blockers)
	}
	if len(preview.References) != 1 || !preview.References[0].Resolved {
		t.Fatalf("reference should resolve after split: %+v", preview.References)
	}
	if preview.References[0].After != "api.internal" {
		t.Fatalf("reference should follow the endpoint split, got %q", preview.References[0].After)
	}
	if preview.Fingerprint == "" {
		t.Fatal("preview must carry a group fingerprint")
	}
}

func TestPreviewGroupReportsIdentityChangeAcrossFiles(t *testing.T) {
	engine := newGroupEngine(t)
	group := Group{
		"app": mustDoc("v2", map[string]any{"host": "api.internal"}),
		"lb":  mustDoc("v2", map[string]any{"upstream": "api.internal"}),
	}
	targets := map[string]string{"app": "v3", "lb": "v3"}
	refs := []Reference{{Name: "backend", From: FileField{"app", "host"}, To: FileField{"lb", "upstream"}}}
	preview, err := engine.PreviewGroup(context.Background(), group, targets, refs)
	if err != nil {
		t.Fatalf("PreviewGroup: %v", err)
	}
	if len(preview.Blockers) != 0 {
		t.Fatalf("unexpected blockers: %v", preview.Blockers)
	}
	outcome := preview.References[0]
	if !outcome.Resolved || outcome.Before != "api.internal" || outcome.After != "new-api.internal" {
		t.Fatalf("identity change should be tracked across files: %+v", outcome)
	}
}

func TestBrokenReferenceBlocksWholeGroup(t *testing.T) {
	engine := newGroupEngine(t)
	group, targets, refs := groupFixture()
	group["lb"]["backend_host"] = "other.host"
	store := NewGroupStore(filepath.Join(t.TempDir(), "state"))
	preview, err := engine.PreviewGroup(context.Background(), group, targets, refs)
	if err != nil {
		t.Fatalf("PreviewGroup: %v", err)
	}
	if len(preview.Blockers) == 0 {
		t.Fatal("mismatched reference must block the group")
	}
	_, err = engine.ApplyGroup(context.Background(), group, targets, refs, preview, store.Publish())
	if !errors.Is(err, ErrGroupBlocked) {
		t.Fatalf("expected ErrGroupBlocked, got %v", err)
	}
	if _, loadErr := store.Load(); !errors.Is(loadErr, ErrNotFound) {
		t.Fatalf("nothing may be published for a blocked group, got %v", loadErr)
	}
}

func TestApplyGroupRejectsStaleConfirmation(t *testing.T) {
	engine := newGroupEngine(t)
	group, targets, refs := groupFixture()
	preview, err := engine.PreviewGroup(context.Background(), group, targets, refs)
	if err != nil {
		t.Fatalf("PreviewGroup: %v", err)
	}
	group["app"]["timeout"] = 5
	_, err = engine.ApplyGroup(context.Background(), group, targets, refs, preview, nil)
	if !errors.Is(err, ErrStalePreview) {
		t.Fatalf("expected ErrStalePreview, got %v", err)
	}
}

func TestGroupStorePublishesAtomicallyAndKeepsHistory(t *testing.T) {
	engine := newGroupEngine(t)
	group, targets, refs := groupFixture()
	dir := filepath.Join(t.TempDir(), "state")
	store := NewGroupStore(dir)

	preview, err := engine.PreviewGroup(context.Background(), group, targets, refs)
	if err != nil {
		t.Fatalf("PreviewGroup: %v", err)
	}
	if err := store.RecordPreview(preview); err != nil {
		t.Fatalf("RecordPreview: %v", err)
	}
	if _, err := engine.ApplyGroup(context.Background(), group, targets, refs, preview, store.Publish()); err != nil {
		t.Fatalf("ApplyGroup: %v", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Documents["app"]["host"] != "api.internal" || state.Documents["app"]["version"] != "v2" {
		t.Fatalf("unexpected current group: %v", state.Documents["app"])
	}
	if len(state.Previews) != 2 || state.Previews[0].Preview.Fingerprint != preview.Fingerprint {
		t.Fatalf("historical previews with their basis must remain queryable: %+v", state.Previews)
	}

	// Simulate an interrupted second publish: a complete orphan generation
	// directory exists but the manifest was never switched.
	if err := os.MkdirAll(filepath.Join(dir, "generations", "g000002"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "generations", "g000002", "app.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err = store.Load()
	if err != nil {
		t.Fatalf("Load after interruption: %v", err)
	}
	if state.Generation != "g000001" {
		t.Fatalf("interrupted publish must leave the complete old group current, got %q", state.Generation)
	}

	// Publish a second complete generation, then roll the whole group back.
	preview2, err := engine.PreviewGroup(context.Background(), state.Documents, map[string]string{"app": "v3", "lb": "v3"}, nil)
	if err != nil {
		t.Fatalf("PreviewGroup 2: %v", err)
	}
	if _, err := engine.ApplyGroup(context.Background(), state.Documents, map[string]string{"app": "v3", "lb": "v3"}, nil, preview2, store.Publish()); err != nil {
		t.Fatalf("ApplyGroup 2: %v", err)
	}
	report, err := store.Rollback("g000001")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(report.Restored) != 2 {
		t.Fatalf("rollback must restore every file, got %v", report.Restored)
	}
	if len(report.Warnings) == 0 || !strings.Contains(report.Warnings[0], "g000002") {
		t.Fatalf("rollback must state what is not restored: %v", report.Warnings)
	}
	state, err = store.Load()
	if err != nil {
		t.Fatalf("Load after rollback: %v", err)
	}
	if state.Generation != "g000001" || state.Documents["app"]["version"] != "v2" {
		t.Fatalf("rollback must expose the complete old group, got %q", state.Generation)
	}
	if _, err := os.Stat(filepath.Join(dir, "generations", "g000002")); err != nil {
		t.Fatalf("original generations must be preserved: %v", err)
	}
}

func TestRollbackGroupIsAllOrNothing(t *testing.T) {
	engine := newGroupEngine(t)
	group := Group{
		"app": mustDoc("v2", map[string]any{"host": "api.internal"}),
		"lb":  mustDoc("v2", map[string]any{"backend_host": "api.internal"}),
	}
	targets := map[string]string{"app": "v1", "lb": "v1"}
	refs := []Reference{{Name: "backend", From: FileField{"app", "endpoint"}, To: FileField{"lb", "backend_host"}}}
	result, err := engine.RollbackGroup(context.Background(), group, targets, refs)
	if err != nil {
		t.Fatalf("RollbackGroup: %v", err)
	}
	if result.Documents["app"]["endpoint"] != "api.internal" || result.Documents["app"]["version"] != "v1" {
		t.Fatalf("unexpected rollback result: %v", result.Documents["app"])
	}
	if len(result.Blockers) != 0 {
		t.Fatalf("references should resolve after rollback: %v", result.Blockers)
	}

	// The lb file has no reverse step: the whole group rollback is refused.
	engine2 := NewEngine()
	engine2.MustRegister(Step{
		From: "v1", To: "v2",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			out := doc.Clone()
			out["version"] = "v2"
			return out, nil
		},
	})
	_, err = engine2.RollbackGroup(context.Background(), group, targets, refs)
	if !errors.Is(err, ErrReverseUnsupported) {
		t.Fatalf("expected ErrReverseUnsupported, got %v", err)
	}
	if !strings.Contains(err.Error(), "lb") {
		t.Fatalf("blocker must name the file that cannot roll back: %v", err)
	}
}
