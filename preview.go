package configmigrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
)

type FieldSource string

const (
	SourcePreserved FieldSource = "preserved"
	SourceDefault   FieldSource = "default"
	SourceSplit     FieldSource = "split"
	SourceDerived   FieldSource = "derived"
	SourceAdded     FieldSource = "added"
)

type FieldEffect struct {
	Field  string
	Source FieldSource
	Detail string
	// From lists the pre-step fields this output field is derived from. It lets
	// merges follow renames, splits, and joins by common origin instead of
	// matching post-migration names.
	From []string
}

type FieldProvenance struct {
	Field  string      `json:"field"`
	Source FieldSource `json:"source"`
	Detail string      `json:"detail,omitempty"`
	Step   string      `json:"step"`
}

type Preview struct {
	FromVersion string            `json:"from_version"`
	ToVersion   string            `json:"to_version"`
	Path        []string          `json:"path"`
	Fields      []FieldProvenance `json:"fields"`
	Losses      []string          `json:"losses,omitempty"`
	Fingerprint string            `json:"fingerprint"`
}

func fingerprint(doc Document, target string, path []string) string {
	payload, _ := json.Marshal(map[string]any{
		"document": doc,
		"target":   target,
		"path":     path,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) Preview(ctx context.Context, doc Document, target string, options ...Option) (Preview, error) {
	if doc == nil {
		return Preview{}, fmt.Errorf("%w: document is required", ErrInvalidStep)
	}
	from, ok := doc["version"].(string)
	if !ok || from == "" {
		return Preview{}, fmt.Errorf("%w: document is missing a string version field", ErrInvalidStep)
	}
	if from == target {
		return Preview{}, fmt.Errorf("%w: %q", ErrAlreadyAtTarget, from)
	}

	opts := migrateOptions{now: timeNow}
	for _, option := range options {
		option(&opts)
	}
	versions, err := e.resolvePath(from, target, opts.explicitPath)
	if err != nil {
		return Preview{}, err
	}
	steps := make([]Step, 0, len(versions)-1)
	for i := 0; i < len(versions)-1; i++ {
		steps = append(steps, e.steps[versions[i]][versions[i+1]])
	}

	preview := Preview{
		FromVersion: from,
		ToVersion:   target,
		Path:        append([]string(nil), versions...),
	}
	working := doc.Clone()
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return Preview{}, err
		}
		next, err := runStep(ctx, step, working)
		if err != nil {
			return Preview{}, fmt.Errorf("migration %s -> %s failed: %w", step.From, step.To, err)
		}
		if next == nil {
			return Preview{}, fmt.Errorf("migration %s -> %s failed: returned a nil document", step.From, step.To)
		}
		fields, losses := stepProvenance(step, working, next)
		preview.Fields = append(preview.Fields, fields...)
		preview.Losses = append(preview.Losses, losses...)
		working = next
	}
	preview.Fingerprint = fingerprint(doc, target, preview.Path)
	return preview, nil
}

func stepProvenance(step Step, before, after Document) ([]FieldProvenance, []string) {
	effects := make(map[string]FieldEffect, len(step.Effects))
	for _, effect := range step.Effects {
		effects[effect.Field] = effect
	}
	label := step.From + " -> " + step.To

	var fields []FieldProvenance
	var losses []string
	for key, afterValue := range after {
		beforeValue, existed := before[key]
		effect, declared := effects[key]
		switch {
		case !existed:
			source := SourceAdded
			if declared && effect.Source != "" {
				source = effect.Source
			}
			fields = append(fields, FieldProvenance{Field: key, Source: source, Detail: effect.Detail, Step: label})
		case !reflect.DeepEqual(beforeValue, afterValue):
			source := SourceDerived
			if declared && effect.Source != "" {
				source = effect.Source
			}
			fields = append(fields, FieldProvenance{Field: key, Source: source, Detail: effect.Detail, Step: label})
		default:
			fields = append(fields, FieldProvenance{Field: key, Source: SourcePreserved, Detail: effect.Detail, Step: label})
		}
	}
	for key := range before {
		if _, kept := after[key]; kept {
			continue
		}
		detail := ""
		if effect, declared := effects[key]; declared {
			detail = effect.Detail
		}
		loss := fmt.Sprintf("field %q removed by step %s", key, label)
		if detail != "" {
			loss += " (" + detail + ")"
		}
		losses = append(losses, loss)
	}
	return fields, losses
}

func (e *Engine) Apply(ctx context.Context, doc Document, preview Preview, publish func(context.Context, Release) error, options ...Option) (Result, error) {
	if doc == nil {
		return Result{}, fmt.Errorf("%w: document is required", ErrInvalidStep)
	}
	from, _ := doc["version"].(string)
	if from != preview.FromVersion {
		return Result{}, fmt.Errorf("%w: preview was computed for version %q but input is %q", ErrStalePreview, preview.FromVersion, from)
	}
	if fingerprint(doc, preview.ToVersion, preview.Path) != preview.Fingerprint {
		return Result{}, fmt.Errorf("%w: input changed after the preview was computed; run Preview again", ErrStalePreview)
	}
	applyOptions := append([]Option{WithPath(preview.Path...)}, options...)
	return e.Publish(ctx, doc, preview.ToVersion, publish, applyOptions...)
}
