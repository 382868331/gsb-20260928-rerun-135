package configmigrator

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newProvenanceEngine(t *testing.T) *Engine {
	t.Helper()
	engine := NewEngine()
	engine.MustRegister(Step{
		From:        "v1",
		To:          "v2",
		Description: "add default timeout",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			next := doc.Clone()
			next["version"] = "v2"
			if _, ok := next["timeout_seconds"]; !ok {
				next["timeout_seconds"] = 30
			}
			return next, nil
		},
		Effects: []FieldEffect{
			{Field: "timeout_seconds", Source: SourceDefault, Detail: "default 30s when unset"},
		},
	})
	engine.MustRegister(Step{
		From:        "v2",
		To:          "v3",
		Description: "split endpoint",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			endpoint, ok := doc["endpoint"].(string)
			if !ok {
				return nil, errors.New("endpoint is required")
			}
			next := doc.Clone()
			next["version"] = "v3"
			next["host"] = endpoint
			next["port"] = 443
			delete(next, "endpoint")
			return next, nil
		},
		Effects: []FieldEffect{
			{Field: "host", Source: SourceSplit, Detail: "split from endpoint"},
			{Field: "port", Source: SourceSplit, Detail: "split from endpoint, default 443"},
			{Field: "endpoint", Detail: "split into host and port"},
		},
	})
	return engine
}

func findProvenance(preview Preview, field string) (FieldProvenance, bool) {
	for _, f := range preview.Fields {
		if f.Field == field {
			return f, true
		}
	}
	return FieldProvenance{}, false
}

func TestPreviewExplainsProvenanceAndLoss(t *testing.T) {
	engine := newProvenanceEngine(t)
	doc := mustDoc("v1", map[string]any{"endpoint": "api.example.com", "keep": "me"})

	preview, err := engine.Preview(context.Background(), doc, "v3")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Path[0] != "v1" || preview.Path[len(preview.Path)-1] != "v3" {
		t.Fatalf("unexpected path: %v", preview.Path)
	}
	if host, ok := findProvenance(preview, "host"); !ok || host.Source != SourceSplit {
		t.Fatalf("host should come from a split rule: %+v", host)
	}
	if timeout, ok := findProvenance(preview, "timeout_seconds"); !ok || timeout.Source != SourceDefault {
		t.Fatalf("timeout should come from a default: %+v", timeout)
	}
	if keep, ok := findProvenance(preview, "keep"); !ok || keep.Source != SourcePreserved {
		t.Fatalf("keep should be preserved from the old value: %+v", keep)
	}
	if len(preview.Losses) != 1 || !strings.Contains(preview.Losses[0], "endpoint") {
		t.Fatalf("endpoint removal must be flagged as loss: %v", preview.Losses)
	}
	if preview.Fingerprint == "" {
		t.Fatal("preview must carry a fingerprint")
	}
}

func TestApplyRejectsStalePreview(t *testing.T) {
	engine := newProvenanceEngine(t)
	doc := mustDoc("v1", map[string]any{"endpoint": "api.example.com"})
	preview, err := engine.Preview(context.Background(), doc, "v3")
	if err != nil {
		t.Fatal(err)
	}

	changed := mustDoc("v1", map[string]any{"endpoint": "other.example.com"})
	published := false
	_, err = engine.Apply(context.Background(), changed, preview, func(context.Context, Release) error {
		published = true
		return nil
	})
	if !errors.Is(err, ErrStalePreview) || published {
		t.Fatalf("stale preview must be rejected, err=%v published=%v", err, published)
	}

	result, err := engine.Apply(context.Background(), doc, preview, func(context.Context, Release) error {
		published = true
		return nil
	})
	if err != nil || !published || !result.Migrated {
		t.Fatalf("fresh preview must apply: err=%v published=%v result=%+v", err, published, result)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "endpoint") {
		t.Fatalf("migration result must flag dropped endpoint: %v", result.Warnings)
	}
}

func TestRollbackBlockersAreConcrete(t *testing.T) {
	engine := newProvenanceEngine(t)
	blockers := engine.RollbackBlockers("v2", "v1")
	if len(blockers) != 1 || !strings.Contains(blockers[0], "v1 -> v2") {
		t.Fatalf("expected concrete blocker naming the step, got %v", blockers)
	}

	doc := mustDoc("v2", map[string]any{"endpoint": "api.example.com"})
	_, err := engine.Reverse(context.Background(), doc, "v1")
	if !errors.Is(err, ErrReverseUnsupported) || !strings.Contains(err.Error(), "v1 -> v2") {
		t.Fatalf("reverse error must name the blocking step: %v", err)
	}
	if blockers := engine.RollbackBlockers("v9", "v1"); len(blockers) == 0 {
		t.Fatal("missing path must be reported as a blocker")
	}
}

func TestReverseReportsLossInsteadOfPretendingFullRestore(t *testing.T) {
	engine := NewEngine()
	engine.MustRegister(Step{
		From: "v1",
		To:   "v2",
		Migrate: func(_ context.Context, doc Document) (Document, error) {
			next := doc.Clone()
			next["version"] = "v2"
			next["merged"] = doc["legacy_a"]
			delete(next, "legacy_a")
			delete(next, "legacy_b")
			return next, nil
		},
		Reverse: func(_ context.Context, doc Document) (Document, error) {
			next := doc.Clone()
			next["version"] = "v1"
			next["legacy_a"] = doc["merged"]
			delete(next, "merged")
			delete(next, "extra")
			return next, nil
		},
	})
	doc := mustDoc("v2", map[string]any{"merged": "x", "extra": "dropped"})
	result, err := engine.Reverse(context.Background(), doc, "v1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "extra") {
			found = true
		}
	}
	if !found {
		t.Fatalf("rollback must warn about unrecoverable fields: %v", result.Warnings)
	}
}
