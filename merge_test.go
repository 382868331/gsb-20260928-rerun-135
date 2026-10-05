package configmigrator

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func mergeBase() Document {
	return Document{
		"version": "v1",
		"name":    "billing",
		"address": "1 main st,springfield",
		"legacy":  true,
	}
}

func mergeCandidateA() Document {
	return Document{
		"version":      "v2",
		"service_name": "billing",
		"street":       "1 main st",
		"city":         "springfield",
		"legacy":       true,
		"retries":      3,
	}
}

func mergeCandidateB() Document {
	return Document{
		"version": "v3",
		"name":    "billing",
		"address": "1 main st,springfield",
		"owner":   "team-b",
	}
}

func mergeTestOptions() MergeOptions {
	return MergeOptions{
		OriginA: map[string]string{
			"service_name": "name",
			"street":       "address",
			"city":         "address",
		},
	}
}

func itemByID(t *testing.T, plan MergePlan, id string) MergeItem {
	t.Helper()
	for _, item := range plan.Items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("item %q not found in plan", id)
	return MergeItem{}
}

func TestCompareMergeCorrelatesRenamesAndSplits(t *testing.T) {
	plan, err := CompareMerge(mergeBase(), mergeCandidateA(), mergeCandidateB(), mergeTestOptions())
	if err != nil {
		t.Fatalf("CompareMerge: %v", err)
	}

	renamed := itemByID(t, plan, "name")
	if renamed.Kind != ItemModified {
		t.Fatalf("renamed field kept by B and renamed by A: kind = %s, want %s", renamed.Kind, ItemModified)
	}
	if !reflect.DeepEqual(renamed.FieldsA, []string{"service_name"}) {
		t.Fatalf("renamed field should trace to service_name, got %v", renamed.FieldsA)
	}

	split := itemByID(t, plan, "address")
	if split.Kind != ItemModified {
		t.Fatalf("split field kept by B: kind = %s, want %s", split.Kind, ItemModified)
	}
	if !reflect.DeepEqual(split.FieldsA, []string{"city", "street"}) {
		t.Fatalf("split field should collect both parts, got %v", split.FieldsA)
	}

	removed := itemByID(t, plan, "legacy")
	if removed.Kind != ItemRemoved {
		t.Fatalf("field dropped only by B: kind = %s, want %s", removed.Kind, ItemRemoved)
	}

	addedA := itemByID(t, plan, "retries")
	if addedA.Kind != ItemAdded {
		t.Fatalf("field added only by A: kind = %s, want %s", addedA.Kind, ItemAdded)
	}
	addedB := itemByID(t, plan, "owner")
	if addedB.Kind != ItemAdded {
		t.Fatalf("field added only by B: kind = %s, want %s", addedB.Kind, ItemAdded)
	}
}

func TestCompareMergeDetectsConflict(t *testing.T) {
	base := Document{"version": "v1", "timeout": 30}
	a := Document{"version": "v2", "timeout": 60}
	b := Document{"version": "v3", "timeout": 90}
	plan, err := CompareMerge(base, a, b, MergeOptions{})
	if err != nil {
		t.Fatalf("CompareMerge: %v", err)
	}
	item := itemByID(t, plan, "timeout")
	if item.Kind != ItemConflict {
		t.Fatalf("kind = %s, want %s", item.Kind, ItemConflict)
	}
}

func TestResolveMergeDefaultsAndDecisions(t *testing.T) {
	plan, err := CompareMerge(mergeBase(), mergeCandidateA(), mergeCandidateB(), mergeTestOptions())
	if err != nil {
		t.Fatalf("CompareMerge: %v", err)
	}
	resolution, err := ResolveMerge(plan, nil, mergeTestOptions())
	if err != nil {
		t.Fatalf("ResolveMerge: %v", err)
	}
	if len(resolution.Unresolved) != 0 {
		t.Fatalf("no conflicts expected, got %v", resolution.Unresolved)
	}
	doc := resolution.Document
	if doc["service_name"] != "billing" {
		t.Fatalf("renamed field should come from A, got %v", doc["service_name"])
	}
	if doc["street"] != "1 main st" || doc["city"] != "springfield" {
		t.Fatalf("split fields should come from A, got %v / %v", doc["street"], doc["city"])
	}
	if doc["retries"] != 3 || doc["owner"] != "team-b" {
		t.Fatalf("additions from both sides should be kept, got %v / %v", doc["retries"], doc["owner"])
	}
	if _, ok := doc["legacy"]; ok {
		t.Fatal("field removed by B and untouched by A defaults to the removal")
	}

	override, err := ResolveMerge(plan, []MergeDecision{
		{Item: "legacy", Choice: ChoiceA, Rationale: "legacy flag still needed by team A"},
	}, mergeTestOptions())
	if err != nil {
		t.Fatalf("ResolveMerge with decision: %v", err)
	}
	if override.Document["legacy"] != true {
		t.Fatalf("decision should override the default, got %v", override.Document["legacy"])
	}
	if len(override.Applied) != 1 {
		t.Fatalf("expected 1 applied decision, got %d", len(override.Applied))
	}
}

func TestAdoptMergeRequiresRationaleForConflicts(t *testing.T) {
	base := Document{"version": "v1", "timeout": 30}
	a := Document{"version": "v2", "timeout": 60}
	b := Document{"version": "v3", "timeout": 90}
	plan, err := CompareMerge(base, a, b, MergeOptions{})
	if err != nil {
		t.Fatalf("CompareMerge: %v", err)
	}
	published := false
	publish := func(context.Context, MergeRelease) error { published = true; return nil }

	_, err = AdoptMerge(context.Background(), base, a, b, MergeOptions{}, plan, nil, publish)
	if !errors.Is(err, ErrMergeBlocked) {
		t.Fatalf("unresolved conflict should block adoption, got %v", err)
	}
	_, err = AdoptMerge(context.Background(), base, a, b, MergeOptions{}, plan, []MergeDecision{
		{Item: "timeout", Choice: ChoiceA},
	}, publish)
	if !errors.Is(err, ErrMergeBlocked) {
		t.Fatalf("conflict decision without rationale should block adoption, got %v", err)
	}
	if published {
		t.Fatal("publisher must not run while the merge is blocked")
	}

	resolution, err := AdoptMerge(context.Background(), base, a, b, MergeOptions{}, plan, []MergeDecision{
		{Item: "timeout", Choice: ChoiceA, Rationale: "team A measured the safe upper bound"},
	}, publish)
	if err != nil {
		t.Fatalf("adoption with rationale: %v", err)
	}
	if !published {
		t.Fatal("publisher should run once every conflict has a rationale")
	}
	if resolution.Document["timeout"] != 60 {
		t.Fatalf("merged timeout = %v, want 60", resolution.Document["timeout"])
	}
}

func TestAdoptMergeBlocksOnIncompleteReference(t *testing.T) {
	base := Document{"version": "v1", "id": "svc-1", "parent": "svc-1"}
	a := Document{"version": "v2", "id": "svc-2", "parent": "svc-2"}
	b := Document{"version": "v3", "id": "svc-2"}
	opts := MergeOptions{
		References: []MergeReference{{Name: "parent-link", From: "parent", To: "id"}},
	}
	plan, err := CompareMerge(base, a, b, opts)
	if err != nil {
		t.Fatalf("CompareMerge: %v", err)
	}
	_, err = AdoptMerge(context.Background(), base, a, b, opts, plan, nil, nil)
	if !errors.Is(err, ErrMergeBlocked) {
		t.Fatalf("incomplete reference should block adoption, got %v", err)
	}

	fixed, err := AdoptMerge(context.Background(), base, a, b, opts, plan, []MergeDecision{
		{Item: "parent", Choice: ChoiceA, Rationale: "keep the reference target present"},
	}, nil)
	if err != nil {
		t.Fatalf("adoption with complete reference: %v", err)
	}
	if fixed.Document["parent"] != fixed.Document["id"] {
		t.Fatalf("reference should resolve, got parent=%v id=%v", fixed.Document["parent"], fixed.Document["id"])
	}
}

func TestRecheckDecisionsOnlyFlagsAffectedItems(t *testing.T) {
	base := Document{"version": "v1", "timeout": 30, "region": "eu"}
	a := Document{"version": "v2", "timeout": 60, "region": "eu"}
	b := Document{"version": "v3", "timeout": 90, "region": "us"}
	plan, err := CompareMerge(base, a, b, MergeOptions{})
	if err != nil {
		t.Fatalf("CompareMerge: %v", err)
	}
	decisions := []MergeDecision{
		{Item: "timeout", Choice: ChoiceA, Rationale: "measured bound"},
		{Item: "region", Choice: ChoiceB, Rationale: "team B owns regions"},
	}

	changedB := Document{"version": "v3", "timeout": 120, "region": "us"}
	newPlan, err := CompareMerge(base, a, changedB, MergeOptions{})
	if err != nil {
		t.Fatalf("CompareMerge: %v", err)
	}
	kept, stale := RecheckDecisions(plan, decisions, newPlan)
	if len(kept) != 1 || kept[0].Item != "region" {
		t.Fatalf("only the region decision should carry over, got %v", kept)
	}
	if len(stale) != 1 || stale[0].Item != "timeout" {
		t.Fatalf("only the timeout decision should be stale, got %v", stale)
	}
}

func TestAdoptMergeRejectsStalePlan(t *testing.T) {
	base := Document{"version": "v1", "timeout": 30}
	a := Document{"version": "v2", "timeout": 60}
	b := Document{"version": "v3", "timeout": 90}
	plan, err := CompareMerge(base, a, b, MergeOptions{})
	if err != nil {
		t.Fatalf("CompareMerge: %v", err)
	}
	b["timeout"] = 120
	_, err = AdoptMerge(context.Background(), base, a, b, MergeOptions{}, plan, []MergeDecision{
		{Item: "timeout", Choice: ChoiceA, Rationale: "measured bound"},
	}, nil)
	if !errors.Is(err, ErrStalePreview) {
		t.Fatalf("changed candidate should be rejected as stale, got %v", err)
	}
}

func TestMergeStoreSaveLoadUndoPreservesHistory(t *testing.T) {
	store := NewMergeStore(t.TempDir() + "/merge-state.json")
	state := MergeWorkingState{
		Base:      mergeBase(),
		A:         mergeCandidateA(),
		B:         mergeCandidateB(),
		OriginA:   mergeTestOptions().OriginA,
		Decisions: []MergeDecision{{Item: "legacy", Choice: ChoiceA, Rationale: "still needed"}},
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	changed := state.Clone()
	changed.Decisions = append(changed.Decisions, MergeDecision{Item: "address", Choice: ChoiceB, Rationale: "single field is simpler"})
	if err := store.Save(changed); err != nil {
		t.Fatalf("Save changed: %v", err)
	}

	loaded, history, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Decisions) != 2 {
		t.Fatalf("expected 2 decisions after reopen, got %d", len(loaded.Decisions))
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(history))
	}

	undone, err := store.Undo()
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(undone.Decisions) != 1 {
		t.Fatalf("undo should restore the earlier decisions, got %d", len(undone.Decisions))
	}
	_, history, err = store.Load()
	if err != nil {
		t.Fatalf("Load after undo: %v", err)
	}
	if len(history) != 3 || history[2].Action != "undo" {
		t.Fatalf("undo must be appended to history, got %v", history)
	}
	first := history[0].State
	if first.Base["name"] != "billing" || first.A["service_name"] != "billing" || first.B["owner"] != "team-b" {
		t.Fatal("original candidates must remain intact in history")
	}
	if len(history[1].State.Decisions) != 2 {
		t.Fatal("undone decisions must remain recorded in history")
	}
}

func TestMergeStorePublishRecordsAdoption(t *testing.T) {
	store := NewMergeStore(t.TempDir() + "/merge-state.json")
	base := Document{"version": "v1", "timeout": 30}
	a := Document{"version": "v2", "timeout": 60}
	b := Document{"version": "v3", "timeout": 90}
	state := MergeWorkingState{Base: base, A: a, B: b}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	plan, err := CompareMerge(base, a, b, MergeOptions{})
	if err != nil {
		t.Fatalf("CompareMerge: %v", err)
	}
	decisions := []MergeDecision{{Item: "timeout", Choice: ChoiceB, Rationale: "higher bound is safer"}}
	resolution, err := AdoptMerge(context.Background(), base, a, b, MergeOptions{}, plan, decisions, store.Publish(state))
	if err != nil {
		t.Fatalf("AdoptMerge: %v", err)
	}
	if resolution.Document["timeout"] != 90 {
		t.Fatalf("merged timeout = %v, want 90", resolution.Document["timeout"])
	}
	_, history, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	last := history[len(history)-1]
	if last.Action != "adopt" || last.Release == nil {
		t.Fatalf("adoption should be recorded with its release, got %+v", last)
	}
	if last.Release.A["timeout"] != 60.0 || last.Release.B["timeout"] != 90.0 {
		t.Fatal("release must preserve both original candidates")
	}
	if len(last.Release.Decisions) != 1 || last.Release.Decisions[0].Rationale == "" {
		t.Fatal("release must preserve the human decisions and rationales")
	}
}
