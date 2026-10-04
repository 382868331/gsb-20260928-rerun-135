package configmigrator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newMergeEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine()
	e.MustRegister(Step{
		From: "v1", To: "v2", Description: "rename endpoint to host",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			out := doc.Clone()
			out["version"] = "v2"
			if endpoint, ok := out["endpoint"]; ok {
				out["host"] = endpoint
				delete(out, "endpoint")
			}
			return out, nil
		},
		Effects: []FieldEffect{{Field: "host", Source: SourceDerived, From: []string{"endpoint"}}},
	})
	e.MustRegister(Step{
		From: "v1", To: "v2alt", Description: "rename endpoint to server_name",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			out := doc.Clone()
			out["version"] = "v2alt"
			if endpoint, ok := out["endpoint"]; ok {
				out["server_name"] = endpoint
				delete(out, "endpoint")
			}
			return out, nil
		},
		Effects: []FieldEffect{{Field: "server_name", Source: SourceDerived, From: []string{"endpoint"}}},
	})
	return e
}

func mergeFixture() (MergeRequest, []MergeRef) {
	req := MergeRequest{
		Base: mustDoc("v1", map[string]any{"endpoint": "api.internal", "timeout": 30}),
		A: MergeCandidate{
			Document: mustDoc("v2", map[string]any{"host": "api.internal", "timeout": 30}),
			From:     "v1", Path: []string{"v1", "v2"},
		},
		B: MergeCandidate{
			Document: mustDoc("v2alt", map[string]any{"server_name": "api.internal", "timeout": 30}),
			From:     "v1", Path: []string{"v1", "v2alt"},
		},
	}
	refs := []MergeRef{{
		Name:  "endpoint",
		FromA: FileField{File: "a", Field: "host"},
		ToA:   FileField{File: "a", Field: "host"},
		FromB: FileField{File: "b", Field: "server_name"},
		ToB:   FileField{File: "b", Field: "server_name"},
	}}
	return req, refs
}

func TestMergeAlignsRenamedObjectByCommonOrigin(t *testing.T) {
	e := newMergeEngine(t)
	req, _ := mergeFixture()
	session, err := e.NewMergeSession(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("NewMergeSession: %v", err)
	}
	preview := session.Preview()
	var endpoint *MergeItem
	for i := range preview.Items {
		if preview.Items[i].ID == "origin:endpoint" {
			endpoint = &preview.Items[i]
		}
	}
	if endpoint == nil {
		t.Fatalf("renamed endpoint must stay one object, got %+v", preview.Items)
	}
	if len(endpoint.Fields[SideA]) != 1 || endpoint.Fields[SideA][0] != "host" || len(endpoint.Fields[SideB]) != 1 || endpoint.Fields[SideB][0] != "server_name" {
		t.Fatalf("both post-migration names must be visible: %+v", endpoint.Fields)
	}
	if endpoint.Status != MergeAuto {
		t.Fatalf("unchanged same object must auto-resolve, got %s", endpoint.Status)
	}
}

func TestMergeDistinguishesAddDeleteAndConflictingModify(t *testing.T) {
	e := newMergeEngine(t)
	req, _ := mergeFixture()
	req.A.Document["new_on_a"] = "x"
	delete(req.B.Document, "timeout")
	req.A.Document["timeout"] = 60
	req.B.Document["timeout"] = 45
	session, err := e.NewMergeSession(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("NewMergeSession: %v", err)
	}
	byID := map[string]MergeItem{}
	for _, item := range session.Preview().Items {
		byID[item.ID] = item
	}
	added, ok := byID["new:new_on_a"]
	if !ok || added.Kind != "add" || added.Status != MergeAuto {
		t.Fatalf("one-sided addition must be classified as an automatic add: %+v", added)
	}
	if item := byID["origin:timeout"]; item.Kind != "modify" || item.Status != MergeUnresolved {
		t.Fatalf("both sides changing the same meaning differently must conflict: %+v", item)
	}
}

func TestMergeResolutionRequiresReasonAndBlocksCommit(t *testing.T) {
	e := newMergeEngine(t)
	req, _ := mergeFixture()
	req.A.Document["timeout"] = 60
	req.B.Document["timeout"] = 45
	session, err := e.NewMergeSession(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("NewMergeSession: %v", err)
	}
	if err := session.Resolve("origin:timeout", "a", "", nil); !errors.Is(err, ErrInvalidMergeDecision) {
		t.Fatalf("resolution without reason must fail, got %v", err)
	}
	if _, err := session.Commit(context.Background(), nil); !errors.Is(err, ErrMergeBlocked) {
		t.Fatalf("unresolved conflict must block adoption, got %v", err)
	}
	if err := session.Resolve("origin:timeout", "a", "team A keeps the longer timeout", nil); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	result, err := session.Commit(context.Background(), nil)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if result.Document["timeout"] != 60 || result.Document["host"] != "api.internal" {
		t.Fatalf("merged document must use the chosen value and keep the shared object: %v", result.Document)
	}
}

func TestMergeBrokenCrossReferenceBlocksAdoption(t *testing.T) {
	e := newMergeEngine(t)
	req, _ := mergeFixture()
	req.Base["peer"] = "api.internal"
	req.A.Document["peer"] = "api.internal"
	req.B.Document["peer"] = "other.internal"
	refs := []MergeRef{{
		Name:  "peer-link",
		FromA: FileField{File: "a", Field: "peer"},
		ToA:   FileField{File: "a", Field: "host"},
		FromB: FileField{File: "b", Field: "peer"},
		ToB:   FileField{File: "b", Field: "server_name"},
	}}
	session, err := e.NewMergeSession(context.Background(), req, refs)
	if err != nil {
		t.Fatalf("NewMergeSession: %v", err)
	}
	if len(session.Preview().Refs) != 1 || session.Preview().Refs[0].BOK {
		t.Fatalf("candidate B reference mismatch must be shown: %+v", session.Preview().Refs)
	}
	if _, err := session.Commit(context.Background(), nil); !errors.Is(err, ErrMergeBlocked) {
		t.Fatalf("broken cross-reference must block adoption, got %v", err)
	}
}

func TestMergeUpdateCandidatesOnlyInvalidatesAffectedDecision(t *testing.T) {
	e := newMergeEngine(t)
	req, _ := mergeFixture()
	req.A.Document["timeout"] = 60
	req.B.Document["timeout"] = 45
	req.A.Document["retries"] = 3
	req.B.Document["retries"] = 5
	session, err := e.NewMergeSession(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Resolve("origin:timeout", "a", "reasoned timeout", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Resolve("new:retries", "b", "reasoned retries", nil); err != nil {
		t.Fatal(err)
	}
	// Team A keeps editing only the timeout object.
	updated := req.A.Document.Clone()
	updated["timeout"] = 90
	preview, err := session.UpdateCandidates(updated, nil)
	if err != nil {
		t.Fatalf("UpdateCandidates: %v", err)
	}
	for _, item := range preview.Items {
		switch item.ID {
		case "origin:timeout":
			if item.Status != MergeStale {
				t.Fatalf("decision based on changed value must be re-checked: %+v", item)
			}
		case "new:retries":
			if item.Status != MergeResolved {
				t.Fatalf("unrelated finished choice must be preserved: %+v", item)
			}
		}
	}
}

func TestMergeStoreReopenUndoAndAuditTrail(t *testing.T) {
	e := newMergeEngine(t)
	dir := filepath.Join(t.TempDir(), "merges")
	store := NewMergeStore(dir)
	store.BindEngine(e)
	req, _ := mergeFixture()
	req.A.Document["timeout"] = 60
	req.B.Document["timeout"] = 45
	session, err := e.NewMergeSession(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	const id = "merge-1"
	if err := store.Save(session, id, "open", "", "", "", nil); err != nil {
		t.Fatalf("save open: %v", err)
	}
	if err := session.Resolve("origin:timeout", "b", "B value matches operations policy", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(session, id, "resolve", "origin:timeout", "b", "B value matches operations policy", nil); err != nil {
		t.Fatalf("save resolve: %v", err)
	}
	session.SetAdoptHook(store.AdoptHook(id))
	if _, err := session.Commit(context.Background(), store.Publish(id)); err != nil {
		t.Fatalf("commit: %v", err)
	}
	official, err := store.Official()
	if err != nil {
		t.Fatalf("official: %v", err)
	}
	if official["timeout"] != float64(45) {
		t.Fatalf("official document must carry the chosen value, got %v", official["timeout"])
	}
	reopened, _, err := store.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reopened.Adopted() {
		t.Fatal("reopened session must remember adoption until explicitly reopened")
	}
	reopened.Reopen()
	undone, err := store.Undo(context.Background(), id, "reconsider timeout choice")
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	item, err := undone.Item("origin:timeout")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != MergeUnresolved {
		t.Fatalf("undo must return the decision to review while keeping the audit trail: %+v", item)
	}
	events, err := store.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 4 {
		t.Fatalf("open, resolve, undo must all be retained with their reasons, got %d", len(events))
	}
	if events[0].Request.A.Document["timeout"] != float64(60) {
		t.Fatal("the original candidates must be preserved in the first audit event")
	}
}

func TestMergeSplitObjectIsShownAsOneItem(t *testing.T) {
	e := NewEngine()
	e.MustRegister(Step{
		From: "v1", To: "v2", Description: "split host and port",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			out := doc.Clone()
			out["version"] = "v2"
			if endpoint, ok := out["endpoint"].(string); ok {
				out["host"] = endpoint
				out["port"] = 443
				delete(out, "endpoint")
			}
			return out, nil
		},
		Effects: []FieldEffect{
			{Field: "host", Source: SourceSplit, From: []string{"endpoint"}},
			{Field: "port", Source: SourceSplit, From: []string{"endpoint"}},
		},
	})
	req := MergeRequest{
		Base: mustDoc("v1", map[string]any{"endpoint": "api.internal"}),
		A:    MergeCandidate{Document: mustDoc("v2", map[string]any{"host": "api.internal", "port": 443}), From: "v1", Path: []string{"v1", "v2"}},
		B:    MergeCandidate{Document: mustDoc("v2", map[string]any{"host": "api.internal", "port": 443}), From: "v1", Path: []string{"v1", "v2"}},
	}
	session, err := e.NewMergeSession(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	var split *MergeItem
	for i := range session.Preview().Items {
		if session.Preview().Items[i].ID == "origin:endpoint" {
			split = &session.Preview().Items[i]
		}
	}
	if split == nil || !split.Split {
		t.Fatalf("split object must be marked and traced to one origin: %+v", session.Preview().Items)
	}
}

func TestMergeRenamedObjectKeepsBothNamesInDraft(t *testing.T) {
	e := newMergeEngine(t)
	req, refs := mergeFixture()
	session, err := e.NewMergeSession(context.Background(), req, refs)
	if err != nil {
		t.Fatal(err)
	}
	draft := session.Draft()
	if draft["host"] != "api.internal" {
		t.Fatalf("draft must keep side A name for its references: %v", draft)
	}
	if draft["server_name"] != "api.internal" {
		t.Fatalf("draft must keep side B name for its references: %v", draft)
	}
	if count := countKeys(draft, "host", "server_name"); count != 2 {
		t.Fatalf("one logical object exposed under both post-migration names, never duplicated values")
	}
}

func countKeys(doc Document, keys ...string) int {
	count := 0
	for _, key := range keys {
		if _, ok := doc[key]; ok {
			count++
		}
	}
	return count
}
