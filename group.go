package configmigrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Group is a set of named configuration files that reference each other and
// must migrate, publish, and roll back as one unit.
type Group map[string]Document

// Clone returns an independent copy of every file in the group.
func (g Group) Clone() Group {
	result := make(Group, len(g))
	for name, doc := range g {
		result[name] = doc.Clone()
	}
	return result
}

// FileField points at one field inside one file of a group.
type FileField struct {
	File  string `json:"file"`
	Field string `json:"field"`
}

func (f FileField) String() string { return f.File + "." + f.Field }

// Reference declares that From must resolve to the same value as To after
// migration. References let previews explain how cross-file links follow
// field splits, merges, or identity changes.
type Reference struct {
	Name string    `json:"name"`
	From FileField `json:"from"`
	To   FileField `json:"to"`
}

// ReferenceOutcome describes how one reference survives a group migration.
type ReferenceOutcome struct {
	Reference Reference `json:"reference"`
	Resolved  bool      `json:"resolved"`
	Before    string    `json:"before,omitempty"`
	After     string    `json:"after,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// GroupPreview explains a coordinated migration of a whole group before it
// happens. Blockers lists every reason the group cannot be published; a
// preview with any blocker must never be applied.
type GroupPreview struct {
	Files       map[string]Preview `json:"files"`
	References  []ReferenceOutcome `json:"references"`
	Blockers    []string           `json:"blockers,omitempty"`
	Fingerprint string             `json:"fingerprint"`
	ComputedAt  time.Time          `json:"computed_at"`
}

// GroupRelease is handed to the group publisher only after every file in the
// group migrated successfully and every reference still resolves.
type GroupRelease struct {
	Originals Group               `json:"originals"`
	Documents Group               `json:"documents"`
	Records   map[string][]Record `json:"records"`
	Preview   GroupPreview        `json:"preview"`
}

// GroupResult describes an in-memory group migration or rollback.
type GroupResult struct {
	Documents Group
	Records   map[string][]Record
	Migrated  bool
	Warnings  []string
	Blockers  []string
}

func sortedNames(g Group) []string {
	names := make([]string, 0, len(g))
	for name := range g {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// groupFingerprint binds a confirmation to the exact inputs: every file
// content, every target version, and every declared reference. Changing any
// of them invalidates the confirmation.
func groupFingerprint(g Group, targets map[string]string, refs []Reference) string {
	canonical := make([]any, 0, len(g))
	for _, name := range sortedNames(g) {
		canonical = append(canonical, map[string]any{"file": name, "document": g[name], "target": targets[name]})
	}
	payload, _ := json.Marshal(map[string]any{"group": canonical, "references": refs})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func resolveField(g Group, ref FileField) (any, bool) {
	doc, ok := g[ref.File]
	if !ok {
		return nil, false
	}
	value, ok := doc[ref.Field]
	return value, ok
}

func checkReference(g Group, ref Reference) (value any, problem string) {
	from, ok := resolveField(g, ref.From)
	if !ok {
		return nil, fmt.Sprintf("%s: %s is missing", ref.Name, ref.From)
	}
	to, ok := resolveField(g, ref.To)
	if !ok {
		return nil, fmt.Sprintf("%s: %s is missing", ref.Name, ref.To)
	}
	if !reflect.DeepEqual(from, to) {
		return from, fmt.Sprintf("%s: %s=%v no longer matches %s=%v", ref.Name, ref.From, from, ref.To, to)
	}
	return from, ""
}

// PreviewGroup migrates every file on a private copy and shows how the
// declared cross-file references follow the migration. Individual file
// failures and unresolvable references become blockers instead of aborting
// the whole preview, so the user sees the complete picture.
func (e *Engine) PreviewGroup(ctx context.Context, g Group, targets map[string]string, refs []Reference, options ...Option) (GroupPreview, error) {
	if len(g) == 0 {
		return GroupPreview{}, fmt.Errorf("%w: group is empty", ErrInvalidStep)
	}
	for name, doc := range g {
		if doc == nil {
			return GroupPreview{}, fmt.Errorf("%w: file %q is missing", ErrInvalidStep, name)
		}
		if targets[name] == "" {
			return GroupPreview{}, fmt.Errorf("%w: file %q has no target version", ErrInvalidStep, name)
		}
	}

	preview := GroupPreview{
		Files:       make(map[string]Preview, len(g)),
		Fingerprint: groupFingerprint(g, targets, refs),
		ComputedAt:  timeNow().UTC(),
	}
	migrated := make(Group, len(g))
	for _, name := range sortedNames(g) {
		if err := ctx.Err(); err != nil {
			return GroupPreview{}, err
		}
		filePreview, err := e.Preview(ctx, g[name], targets[name], options...)
		if err != nil {
			preview.Blockers = append(preview.Blockers, fmt.Sprintf("file %q: %v", name, err))
			continue
		}
		preview.Files[name] = filePreview
		result, err := e.Migrate(ctx, g[name], targets[name], options...)
		if err != nil {
			preview.Blockers = append(preview.Blockers, fmt.Sprintf("file %q: %v", name, err))
			continue
		}
		migrated[name] = result.Document
	}

	for _, ref := range refs {
		outcome := ReferenceOutcome{Reference: ref}
		if before, problem := checkReference(g, ref); problem != "" {
			outcome.Detail = "before migration: " + problem
		} else {
			outcome.Before = fmt.Sprintf("%v", before)
		}
		if len(preview.Blockers) == 0 {
			if after, problem := checkReference(migrated, ref); problem != "" {
				outcome.Detail = problem
				preview.Blockers = append(preview.Blockers, "unresolved reference: "+problem)
			} else {
				outcome.Resolved = true
				outcome.After = fmt.Sprintf("%v", after)
				if outcome.Before != outcome.After {
					outcome.Detail = fmt.Sprintf("identity changed %s -> %s and stays consistent across files", outcome.Before, outcome.After)
				}
			}
		}
		preview.References = append(preview.References, outcome)
	}
	return preview, nil
}

// ApplyGroup publishes a confirmed group preview. The fingerprint is
// re-checked first: if any input file changed after the preview, the whole
// group confirmation is stale and must be reviewed again. A preview with
// blockers is never applied, so a half-migrated group can never be published.
func (e *Engine) ApplyGroup(ctx context.Context, g Group, targets map[string]string, refs []Reference, preview GroupPreview, publish func(context.Context, GroupRelease) error, options ...Option) (GroupResult, error) {
	if len(preview.Blockers) > 0 {
		return GroupResult{}, fmt.Errorf("%w: preview has blockers: %s", ErrGroupBlocked, strings.Join(preview.Blockers, "; "))
	}
	if groupFingerprint(g, targets, refs) != preview.Fingerprint {
		return GroupResult{}, fmt.Errorf("%w: an input file changed after the group preview was confirmed; run PreviewGroup again", ErrStalePreview)
	}
	result := GroupResult{Documents: make(Group, len(g)), Records: make(map[string][]Record, len(g)), Migrated: true}
	for _, name := range sortedNames(g) {
		fileResult, err := e.Migrate(ctx, g[name], targets[name], options...)
		if err != nil {
			return GroupResult{}, fmt.Errorf("file %q: %w", name, err)
		}
		result.Documents[name] = fileResult.Document
		result.Records[name] = fileResult.Records
		result.Warnings = append(result.Warnings, fileResult.Warnings...)
	}
	for _, ref := range refs {
		if _, problem := checkReference(result.Documents, ref); problem != "" {
			return GroupResult{}, fmt.Errorf("%w: %s", ErrGroupBlocked, problem)
		}
	}
	if publish != nil {
		release := GroupRelease{
			Originals: g.Clone(),
			Documents: result.Documents.Clone(),
			Records:   result.Records,
			Preview:   preview,
		}
		if err := publish(ctx, release); err != nil {
			return GroupResult{}, fmt.Errorf("publish group: %w", err)
		}
	}
	return result, nil
}

// RollbackGroup reverses the whole group to the given target versions. It
// refuses to run when any single file cannot be reversed, because rolling
// back only some files would leave the group inconsistent. The result
// reports what was restored and what is irreversibly lost.
func (e *Engine) RollbackGroup(ctx context.Context, g Group, targets map[string]string, refs []Reference, options ...Option) (GroupResult, error) {
	if len(g) == 0 {
		return GroupResult{}, fmt.Errorf("%w: group is empty", ErrInvalidStep)
	}
	var blockers []string
	for _, name := range sortedNames(g) {
		from, _ := g[name]["version"].(string)
		for _, blocker := range e.RollbackBlockers(from, targets[name]) {
			blockers = append(blockers, fmt.Sprintf("file %q: %s", name, blocker))
		}
	}
	if len(blockers) > 0 {
		return GroupResult{}, fmt.Errorf("%w: %s", ErrReverseUnsupported, strings.Join(blockers, "; "))
	}
	result := GroupResult{Documents: make(Group, len(g)), Records: make(map[string][]Record, len(g)), Migrated: true}
	for _, name := range sortedNames(g) {
		fileResult, err := e.Reverse(ctx, g[name], targets[name], options...)
		if err != nil {
			return GroupResult{}, fmt.Errorf("file %q: %w", name, err)
		}
		result.Documents[name] = fileResult.Document
		result.Records[name] = fileResult.Records
		for _, warning := range fileResult.Warnings {
			result.Warnings = append(result.Warnings, fmt.Sprintf("file %q: %s", name, warning))
		}
	}
	for _, ref := range refs {
		if _, problem := checkReference(result.Documents, ref); problem != "" {
			result.Blockers = append(result.Blockers, "after rollback: "+problem)
		}
	}
	return result, nil
}
