package configmigrator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func newGroupEngine(t *testing.T) *Engine {
	t.Helper()
	engine := NewEngine()
	engine.MustRegister(Step{
		From:        "v1",
		To:          "v2",
		Description: "rename identity and drop legacy field",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			next := doc.Clone()
			next["version"] = "v2"
			if name, ok := next["name"].(string); ok {
				next["name"] = name + "-v2"
			}
			delete(next, "legacy")
			return next, nil
		},
	})
	return engine
}

func groupFixture() Group {
	return Group{
		"database": mustDoc("v1", map[string]any{"name": "db-main", "legacy": "x"}),
		"service":  mustDoc("v1", map[string]any{"name": "svc", "database": "db-main"}),
	}
}

func groupSpec() GroupSpec {
	return GroupSpec{References: []Reference{{From: "service", Field: "database", To: "database"}}}
}

func TestPreviewGroupFollowsIdentityChange(t *testing.T) {
	engine := newGroupEngine(t)
	preview, err := engine.PreviewGroup(context.Background(), groupSpec(), groupFixture(), map[string]string{"database": "v2", "service": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Blockers) != 0 {
		t.Fatalf("unexpected blockers: %v", preview.Blockers)
	}
	if len(preview.References) != 1 {
		t.Fatalf("expected one reference preview, got %v", preview.References)
	}
	ref := preview.References[0]
	if ref.Status != ReferenceFollowed || ref.OldValue != "db-main" || ref.NewValue != "db-main-v2" {
		t.Fatalf("reference should follow the identity change: %+v", ref)
	}
	if preview.Fingerprint == "" {
		t.Fatal("group preview must carry a fingerprint")
	}
}

func TestApplyGroupPublishesConsistentGroup(t *testing.T) {
	engine := newGroupEngine(t)
	store := NewGroupStore(filepath.Join(t.TempDir(), "group", "state.json"))
	spec := groupSpec()
	group := groupFixture()

	preview, err := engine.PreviewGroup(context.Background(), spec, group, map[string]string{"database": "v2", "service": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPreview(preview); err != nil {
		t.Fatal(err)
	}
	result, err := engine.ApplyGroup(context.Background(), spec, group, preview, store.PublishGroup())
	if err != nil {
		t.Fatal(err)
	}
	if result.References[0].Status != ReferenceFollowed {
		t.Fatalf("expected followed reference, got %+v", result.References[0])
	}

	current, history, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if current["service"]["database"] != "db-main-v2" {
		t.Fatalf("reference was not rewritten in the published group: %#v", current["service"])
	}
	if current["database"]["name"] != "db-main-v2" {
		t.Fatalf("identity change missing: %#v", current["database"])
	}
	if len(history) != 1 || history[0].Originals["database"]["name"] != "db-main" {
		t.Fatalf("originals were not retained: %+v", history)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("dropped legacy field must be reported as a warning")
	}

	previews, err := store.Previews()
	if err != nil || len(previews) != 1 {
		t.Fatalf("confirmed preview must be queryable after restart: %v %v", previews, err)
	}
	if previews[0].Preview.Fingerprint != preview.Fingerprint {
		t.Fatalf("stored preview lost its basis: %+v", previews[0])
	}
}

func TestApplyGroupRejectsModifiedMember(t *testing.T) {
	engine := newGroupEngine(t)
	spec := groupSpec()
	group := groupFixture()
	preview, err := engine.PreviewGroup(context.Background(), spec, group, map[string]string{"database": "v2", "service": "v2"})
	if err != nil {
		t.Fatal(err)
	}

	changed := group.Clone()
	changed["service"]["database"] = "db-main"
	changed["service"]["replicas"] = 3
	published := false
	_, err = engine.ApplyGroup(context.Background(), spec, changed, preview, func(context.Context, GroupRelease) error {
		published = true
		return nil
	})
	if !errors.Is(err, ErrStalePreview) || published {
		t.Fatalf("modified member must invalidate the whole group confirmation, err=%v published=%v", err, published)
	}
}

func TestBrokenReferenceBlocksWholeGroup(t *testing.T) {
	engine := newGroupEngine(t)
	store := NewGroupStore(filepath.Join(t.TempDir(), "state.json"))
	spec := groupSpec()
	group := groupFixture()
	group["service"]["database"] = "db-missing"

	preview, err := engine.PreviewGroup(context.Background(), spec, group, map[string]string{"database": "v2", "service": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Blockers) != 1 || !strings.Contains(preview.Blockers[0], "db-missing") {
		t.Fatalf("unresolvable reference must be named as a blocker: %v", preview.Blockers)
	}
	if preview.References[0].Status != ReferenceBroken {
		t.Fatalf("reference must be marked broken: %+v", preview.References[0])
	}

	_, err = engine.ApplyGroup(context.Background(), spec, group, preview, store.PublishGroup())
	if !errors.Is(err, ErrGroupBlocked) {
		t.Fatalf("blocked group must not publish, got %v", err)
	}
	if _, _, loadErr := store.Load(); !errors.Is(loadErr, ErrNotFound) {
		t.Fatalf("no half group may be published, load err=%v", loadErr)
	}
}

func TestGroupStoreRollbackRestoresSnapshotAndReportsLoss(t *testing.T) {
	engine := newGroupEngine(t)
	store := NewGroupStore(filepath.Join(t.TempDir(), "state.json"))
	spec := groupSpec()
	group := groupFixture()

	preview, err := engine.PreviewGroup(context.Background(), spec, group, map[string]string{"database": "v2", "service": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ApplyGroup(context.Background(), spec, group, preview, store.PublishGroup()); err != nil {
		t.Fatal(err)
	}
	_, history, err := store.Load()
	if err != nil || len(history) != 1 {
		t.Fatalf("expected one release, got %v %v", history, err)
	}

	rollback, err := store.Rollback(history[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rollback.Restored) != 2 {
		t.Fatalf("both members must be restored: %v", rollback.Restored)
	}
	if rollback.Documents["database"]["version"] != "v2" {
		t.Fatalf("rollback must restore the release snapshot: %#v", rollback.Documents["database"])
	}
	foundLoss := false
	for _, item := range rollback.Unrecoverable {
		if strings.Contains(item, "legacy") {
			foundLoss = true
		}
	}
	if !foundLoss {
		t.Fatalf("dropped field must be reported as not restored: %v", rollback.Unrecoverable)
	}

	current, historyAfter, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if current["database"]["version"] != "v2" {
		t.Fatalf("current group was not rolled back: %#v", current["database"])
	}
	if len(historyAfter) != 1 || historyAfter[0].Originals["database"]["legacy"] != "x" {
		t.Fatalf("rollback must keep originals and history intact: %+v", historyAfter)
	}

	if _, err := store.Rollback("rel-unknown"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("unknown release must be rejected, got %v", err)
	}
}

func TestGroupStorePublisherFailureLeavesPriorGroup(t *testing.T) {
	engine := newGroupEngine(t)
	store := NewGroupStore(filepath.Join(t.TempDir(), "state.json"))
	spec := groupSpec()
	group := groupFixture()
	targets := map[string]string{"database": "v2", "service": "v2"}

	preview, err := engine.PreviewGroup(context.Background(), spec, group, targets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ApplyGroup(context.Background(), spec, group, preview, store.PublishGroup()); err != nil {
		t.Fatal(err)
	}

	// A second confirmed preview whose publisher fails mid-commit must not
	// damage the already published group.
	current, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	engine.MustRegister(Step{
		From: "v2", To: "v3",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			next := doc.Clone()
			next["version"] = "v3"
			return next, nil
		},
	})
	preview2, err := engine.PreviewGroup(context.Background(), spec, current, map[string]string{"database": "v3", "service": "v3"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.ApplyGroup(context.Background(), spec, current, preview2, func(context.Context, GroupRelease) error {
		return errors.New("disk full")
	})
	if err == nil {
		t.Fatal("expected publisher failure")
	}
	after, history, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after["database"]["version"] != "v2" || len(history) != 1 {
		t.Fatalf("interrupted publish must leave the complete old group: %#v history=%d", after["database"], len(history))
	}
	if leftover, _ := filepath.Glob(filepath.Join(filepath.Dir(store.path), ".group-state-*.tmp")); len(leftover) != 0 {
		t.Fatalf("temporary files left behind: %v", leftover)
	}
}
