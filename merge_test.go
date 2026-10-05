package configmigrator

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func mergeFixture() (source, a, b Document, links []OriginLink, refs []MergeReference) {
	source = Document{
		"version":  "v1",
		"endpoint": "example.com:443",
		"timeout":  30,
		"legacy":   true,
	}
	// Team A renamed endpoint to host, tuned timeout, dropped legacy.
	a = Document{
		"version": "v2",
		"host":    "example.com:443",
		"timeout": 45,
	}
	// Team B kept endpoint, changed timeout differently, kept legacy, added retries.
	b = Document{
		"version":  "v2",
		"endpoint": "example.com:443",
		"timeout":  60,
		"legacy":   true,
		"retries":  3,
	}
	links = []OriginLink{
		{Source: "version", A: "version", B: "version"},
		{Source: "endpoint", A: "host", B: "endpoint"}, // renamed on side A
		{Source: "timeout", A: "timeout", B: "timeout"},
		{Source: "legacy", A: "", B: "legacy"},
		{A: "", B: "retries"}, // added only by team B
	}
	refs = []MergeReference{
		{Name: "connection", Items: []string{"endpoint", "timeout"}},
	}
	return source, a, b, links, refs
}

func itemKinds(t *testing.T, preview MergePreview) map[string]MergeItemKind {
	t.Helper()
	kinds := make(map[string]MergeItemKind)
	for _, item := range preview.Items {
		kinds[item.ID] = item.Kind
	}
	return kinds
}

func TestCompareCandidatesClassifiesByMeaning(t *testing.T) {
	source, a, b, links, refs := mergeFixture()
	preview, err := CompareCandidates(source, a, b, links, refs)
	if err != nil {
		t.Fatalf("CompareCandidates: %v", err)
	}
	kinds := itemKinds(t, preview)
	want := map[string]MergeItemKind{
		"version":  ItemAgreed,
		"endpoint": ItemAgreed, // renamed on A but same value: correlated via the link
		"timeout":  ItemConflict,
		"legacy":   ItemRemoved,
		"retries":  ItemAdded,
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	endpoint := preview.item("endpoint")
	if endpoint.Link.A != "host" || endpoint.Link.B != "endpoint" {
		t.Fatalf("rename not traced: %+v", endpoint.Link)
	}
	if got := preview.Pending(); !reflect.DeepEqual(got, []string{"legacy", "retries", "timeout"}) {
		t.Fatalf("pending = %v", got)
	}
}

func TestCompareCandidatesDetectsConflictAfterRename(t *testing.T) {
	source := Document{"endpoint": "a:1"}
	a := Document{"host": "a:2"}     // renamed and changed
	b := Document{"endpoint": "a:3"} // changed differently
	links := []OriginLink{{Source: "endpoint", A: "host", B: "endpoint"}}
	preview, err := CompareCandidates(source, a, b, links, nil)
	if err != nil {
		t.Fatalf("CompareCandidates: %v", err)
	}
	item := preview.item("endpoint")
	if item.Kind != ItemConflict {
		t.Fatalf("kind = %q, want conflict", item.Kind)
	}
}

func TestAdoptMergeRequiresReasonedDecisions(t *testing.T) {
	source, a, b, links, refs := mergeFixture()
	preview, err := CompareCandidates(source, a, b, links, refs)
	if err != nil {
		t.Fatalf("CompareCandidates: %v", err)
	}
	if _, err := AdoptMerge(source, preview, nil); !errors.Is(err, ErrMergeUnresolved) {
		t.Fatalf("err = %v, want ErrMergeUnresolved", err)
	}
	resolutions := []Resolution{
		{ItemID: "timeout", Choice: SideA}, // missing reason
		{ItemID: "legacy", Choice: SideA, Reason: "legacy flag is obsolete"},
		{ItemID: "retries", Choice: SideB, Reason: "retry support is required"},
	}
	if _, err := AdoptMerge(source, preview, resolutions); !errors.Is(err, ErrMergeUnresolved) {
		t.Fatalf("err = %v, want ErrMergeUnresolved for missing reason", err)
	}
	if _, err := AdoptMerge(source, preview, []Resolution{{ItemID: "nope", Choice: SideA, Reason: "x"}}); !errors.Is(err, ErrUnknownMergeItem) {
		t.Fatalf("err = %v, want ErrUnknownMergeItem", err)
	}
}

func TestAdoptMergeBlocksIncompleteReference(t *testing.T) {
	source, a, b, links, refs := mergeFixture()
	preview, err := CompareCandidates(source, a, b, links, refs)
	if err != nil {
		t.Fatalf("CompareCandidates: %v", err)
	}
	// endpoint is agreed, so the reference only needs timeout; resolve the
	// other items and confirm adoption works, then make endpoint conflicted
	// and confirm a partial resolution is blocked.
	resolutions := []Resolution{
		{ItemID: "timeout", Choice: SideA, Reason: "45s matches the new SLA"},
		{ItemID: "legacy", Choice: SideA, Reason: "legacy flag is obsolete"},
		{ItemID: "retries", Choice: SideB, Reason: "retry support is required"},
	}
	merged, err := AdoptMerge(source, preview, resolutions)
	if err != nil {
		t.Fatalf("AdoptMerge: %v", err)
	}
	if merged["host"] != "example.com:443" {
		t.Fatalf("renamed field not adopted under side A name: %v", merged)
	}
	if merged["timeout"] != 45 || merged["retries"] != 3 {
		t.Fatalf("merged = %v", merged)
	}
	if _, ok := merged["legacy"]; ok {
		t.Fatalf("legacy should be removed: %v", merged)
	}
	if _, ok := merged["endpoint"]; ok {
		t.Fatalf("old name endpoint should not linger: %v", merged)
	}

	// Now both teams change the renamed field differently: the reference
	// becomes incomplete when only one of its items is decided.
	a2 := Document{"version": "v2", "host": "a.example:443", "timeout": 45}
	b2 := Document{"version": "v2", "endpoint": "b.example:443", "timeout": 60, "legacy": true, "retries": 3}
	preview2, err := CompareCandidates(source, a2, b2, links, refs)
	if err != nil {
		t.Fatalf("CompareCandidates: %v", err)
	}
	partial := []Resolution{
		{ItemID: "endpoint", Choice: SideA, Reason: "team A host is canonical"},
		{ItemID: "legacy", Choice: SideA, Reason: "legacy flag is obsolete"},
		{ItemID: "retries", Choice: SideB, Reason: "retry support is required"},
		// timeout deliberately undecided
	}
	if _, err := AdoptMerge(source, preview2, partial); !errors.Is(err, ErrMergeUnresolved) {
		t.Fatalf("err = %v, want ErrMergeUnresolved", err)
	}
	mixed := append(partial, Resolution{ItemID: "timeout", Choice: SideB, Reason: "team B timeout"})
	if _, err := AdoptMerge(source, preview2, mixed); !errors.Is(err, ErrMergeBlocked) {
		t.Fatalf("err = %v, want ErrMergeBlocked for mixed sides", err)
	}
	consistent := append(partial, Resolution{ItemID: "timeout", Choice: SideA, Reason: "keep connection settings from one team"})
	merged2, err := AdoptMerge(source, preview2, consistent)
	if err != nil {
		t.Fatalf("AdoptMerge: %v", err)
	}
	if merged2["host"] != "a.example:443" || merged2["timeout"] != 45 {
		t.Fatalf("merged = %v", merged2)
	}
}

func TestRecheckResolutionsOnlyFlagsAffectedItems(t *testing.T) {
	source, a, b, links, refs := mergeFixture()
	preview, err := CompareCandidates(source, a, b, links, refs)
	if err != nil {
		t.Fatalf("CompareCandidates: %v", err)
	}
	resolutions := []Resolution{
		{ItemID: "timeout", Choice: SideA, Reason: "45s matches the new SLA", ItemFingerprint: preview.item("timeout").Fingerprint},
		{ItemID: "legacy", Choice: SideA, Reason: "legacy flag is obsolete", ItemFingerprint: preview.item("legacy").Fingerprint},
		{ItemID: "retries", Choice: SideB, Reason: "retry support is required", ItemFingerprint: preview.item("retries").Fingerprint},
	}
	// Team B changes only the timeout; every other decision must survive.
	b2 := b.Clone()
	b2["timeout"] = 90
	next, report, err := RecheckResolutions(source, a, b2, links, refs, preview, resolutions)
	if err != nil {
		t.Fatalf("RecheckResolutions: %v", err)
	}
	if !reflect.DeepEqual(report.Stale, []string{"timeout"}) {
		t.Fatalf("stale = %v, want [timeout]", report.Stale)
	}
	if !reflect.DeepEqual(report.Valid, []string{"legacy", "retries"}) {
		t.Fatalf("valid = %v", report.Valid)
	}
	if len(report.NewItems) != 0 {
		t.Fatalf("new items = %v", report.NewItems)
	}
	// The stale decision is rejected if reused against the new preview.
	if _, err := AdoptMerge(source, next, resolutions); !errors.Is(err, ErrStalePreview) {
		t.Fatalf("err = %v, want ErrStalePreview", err)
	}
}

func TestMergeSessionSaveReopenUndo(t *testing.T) {
	source, a, b, links, refs := mergeFixture()
	session, err := NewMergeSession(source, a, b, links, refs)
	if err != nil {
		t.Fatalf("NewMergeSession: %v", err)
	}
	a["host"] = "tampered" // caller edits must not leak into the session
	if session.A["host"] != "example.com:443" {
		t.Fatalf("session candidate mutated by caller: %v", session.A)
	}
	if err := session.Resolve("timeout", SideA, "45s matches the new SLA"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := session.Resolve("legacy", SideA, "legacy flag is obsolete"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := session.Resolve("retries", SideB, "retry support is required"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := session.Resolve("ghost", SideA, "no such item"); !errors.Is(err, ErrUnknownMergeItem) {
		t.Fatalf("err = %v, want ErrUnknownMergeItem", err)
	}

	store := NewMergeStore(t.TempDir() + "/merge/session.json")
	if _, err := store.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := store.Save(session); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reopened, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	jsonEqual := func(x, y any) bool {
		bx, _ := json.Marshal(x)
		by, _ := json.Marshal(y)
		return string(bx) == string(by)
	}
	if !jsonEqual(reopened.Source, source) || !jsonEqual(reopened.B, b) || !jsonEqual(reopened.A, Document{
		"version": "v2",
		"host":    "example.com:443",
		"timeout": 45,
	}) {
		t.Fatalf("original candidates not preserved across save/reopen")
	}
	if len(reopened.ActiveResolutions()) != 3 {
		t.Fatalf("active resolutions = %v", reopened.ActiveResolutions())
	}

	// Undo revokes the last decision but keeps it in the log.
	undone, ok := reopened.Undo()
	if !ok || undone.ItemID != "retries" {
		t.Fatalf("undo = %+v, %v", undone, ok)
	}
	if len(reopened.Decisions) != 3 || !reopened.Decisions[2].Revoked {
		t.Fatalf("decision log not preserved: %+v", reopened.Decisions)
	}
	if len(reopened.ActiveResolutions()) != 2 {
		t.Fatalf("active after undo = %v", reopened.ActiveResolutions())
	}
	if _, err := reopened.Adopt(); !errors.Is(err, ErrMergeUnresolved) {
		t.Fatalf("err = %v, want ErrMergeUnresolved after undo", err)
	}

	// Re-decide, adopt, and confirm the adoption is recorded with reasons.
	if err := reopened.Resolve("retries", SideB, "retry support is required"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	merged, err := reopened.Adopt()
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if merged["timeout"] != float64(45) || merged["retries"] != float64(3) {
		t.Fatalf("merged = %v", merged)
	}
	if len(reopened.Adopted) != 1 || len(reopened.Adopted[0].Resolutions) != 3 {
		t.Fatalf("adoptions = %+v", reopened.Adopted)
	}
	for _, res := range reopened.Adopted[0].Resolutions {
		if res.Reason == "" {
			t.Fatalf("adoption lost a reason: %+v", res)
		}
	}

	// A candidate change supersedes the adoption but keeps it, and only the
	// affected decision is flagged.
	b2 := b.Clone()
	b2["timeout"] = 90
	report, err := reopened.Recheck(a, b2)
	if err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	if !reflect.DeepEqual(report.Stale, []string{"timeout"}) {
		t.Fatalf("stale = %v", report.Stale)
	}
	if !reopened.Adopted[0].Superseded {
		t.Fatalf("adoption should be marked superseded")
	}
	if err := store.Save(reopened); err != nil {
		t.Fatalf("Save: %v", err)
	}
	final, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(final.Decisions) != 4 || len(final.Adopted) != 1 {
		t.Fatalf("history not preserved: %+v", final)
	}
	if !strings.Contains(final.Adopted[0].Resolutions[0].Reason, "SLA") {
		t.Fatalf("adoption reasons not preserved: %+v", final.Adopted[0])
	}
}
