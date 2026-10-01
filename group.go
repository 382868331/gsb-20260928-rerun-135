package configmigrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Group is a named set of configuration documents that reference each other
// and must be migrated, validated, and published together.
type Group map[string]Document

// Clone returns an independent copy of every document in the group.
func (g Group) Clone() Group {
	if g == nil {
		return nil
	}
	result := make(Group, len(g))
	for name, doc := range g {
		result[name] = doc.Clone()
	}
	return result
}

// Reference declares that Group[From][Field] holds the identity of member To.
// A document's identity is its "name" field when that is a non-empty string,
// otherwise its key in the group.
type Reference struct {
	From  string `json:"from"`
	Field string `json:"field"`
	To    string `json:"to"`
}

// GroupSpec describes the cross-file references inside a group.
type GroupSpec struct {
	References []Reference
}

func groupIdentity(name string, doc Document) string {
	if value, ok := doc["name"].(string); ok && value != "" {
		return value
	}
	return name
}

// ReferenceStatus describes how one cross-file reference survives a migration.
type ReferenceStatus string

const (
	// ReferenceUnchanged means the target identity did not change.
	ReferenceUnchanged ReferenceStatus = "unchanged"
	// ReferenceFollowed means the target identity changed (rename, split or
	// merge of identity fields) and the reference was rewritten to match.
	ReferenceFollowed ReferenceStatus = "followed"
	// ReferenceBroken means the reference value cannot be matched to any group
	// member; it blocks publishing the group.
	ReferenceBroken ReferenceStatus = "broken"
)

// ReferencePreview shows the fate of one reference across the group migration.
type ReferencePreview struct {
	Reference
	OldValue any             `json:"old_value"`
	NewValue any             `json:"new_value,omitempty"`
	Status   ReferenceStatus `json:"status"`
	Detail   string          `json:"detail,omitempty"`
}

// GroupPreview is the coordinated preview for a whole group. It is only
// publishable when Blockers is empty.
type GroupPreview struct {
	Targets     map[string]string  `json:"targets"`
	Documents   map[string]Preview `json:"documents,omitempty"`
	References  []ReferencePreview `json:"references,omitempty"`
	Blockers    []string           `json:"blockers,omitempty"`
	Fingerprint string             `json:"fingerprint"`
	CreatedAt   time.Time          `json:"created_at"`
}

// GroupResult describes an applied group migration.
type GroupResult struct {
	Documents  map[string]Result
	References []ReferencePreview
	Warnings   []string
}

// GroupRelease is handed to the group publisher only after every member
// migrated successfully and every reference resolved.
type GroupRelease struct {
	Originals          map[string]Document
	Documents          map[string]Document
	Targets            map[string]string
	Records            map[string][]Record
	References         []ReferencePreview
	PreviewFingerprint string
}

func groupFingerprint(group Group, targets map[string]string, previews map[string]Preview) string {
	paths := make(map[string][]string, len(previews))
	for name, preview := range previews {
		paths[name] = preview.Path
	}
	payload, _ := json.Marshal(map[string]any{
		"group":   group,
		"targets": targets,
		"paths":   paths,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func sortedNames(g Group) []string {
	names := make([]string, 0, len(g))
	for name := range g {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// PreviewGroup migrates every member on private copies and reports how each
// cross-file reference follows field splits, merges, or identity changes.
// A reference that cannot be matched to any member is listed in Blockers;
// the group must not be published while blockers remain.
func (e *Engine) PreviewGroup(ctx context.Context, spec GroupSpec, group Group, targets map[string]string, options ...Option) (GroupPreview, error) {
	if len(group) == 0 {
		return GroupPreview{}, fmt.Errorf("%w: group is empty", ErrInvalidStep)
	}
	opts := migrateOptions{now: timeNow}
	for _, option := range options {
		option(&opts)
	}

	migrated := make(Group, len(group))
	previews := make(map[string]Preview)
	resolvedTargets := make(map[string]string, len(group))
	for _, name := range sortedNames(group) {
		doc := group[name]
		if doc == nil {
			return GroupPreview{}, fmt.Errorf("%w: member %q is missing", ErrInvalidStep, name)
		}
		from, ok := doc["version"].(string)
		if !ok || from == "" {
			return GroupPreview{}, fmt.Errorf("%w: member %q is missing a string version field", ErrInvalidStep, name)
		}
		target := targets[name]
		if target == "" {
			target = from
		}
		resolvedTargets[name] = target
		if target == from {
			migrated[name] = doc.Clone()
			continue
		}
		preview, err := e.Preview(ctx, doc, target, options...)
		if err != nil {
			return GroupPreview{}, fmt.Errorf("member %q: %w", name, err)
		}
		previews[name] = preview
		result, err := e.Migrate(ctx, doc, target, options...)
		if err != nil {
			return GroupPreview{}, fmt.Errorf("member %q: %w", name, err)
		}
		migrated[name] = result.Document
	}

	preview := GroupPreview{
		Targets:   resolvedTargets,
		Documents: previews,
		CreatedAt: opts.now().UTC(),
	}
	preview.References = resolveReferences(spec, group, migrated, &preview)
	preview.Fingerprint = groupFingerprint(group, resolvedTargets, previews)
	return preview, nil
}

// resolveReferences compares every declared reference before and after the
// migration, rewriting followed identities into the migrated working copies.
func resolveReferences(spec GroupSpec, before, after Group, preview *GroupPreview) []ReferencePreview {
	oldIdentities := make(map[string]string, len(before))
	newIdentities := make(map[string]string, len(after))
	for name := range before {
		oldIdentities[name] = groupIdentity(name, before[name])
		newIdentities[name] = groupIdentity(name, after[name])
	}
	identityToName := make(map[string]string, len(before))
	for name, identity := range oldIdentities {
		identityToName[identity] = name
	}

	var refs []ReferencePreview
	for _, ref := range spec.References {
		fromDoc, ok := before[ref.From]
		if !ok {
			preview.Blockers = append(preview.Blockers, fmt.Sprintf("reference %s.%s -> %s: source member %q is not in the group", ref.From, ref.Field, ref.To, ref.From))
			continue
		}
		if _, ok := before[ref.To]; !ok {
			preview.Blockers = append(preview.Blockers, fmt.Sprintf("reference %s.%s -> %s: target member %q is not in the group", ref.From, ref.Field, ref.To, ref.To))
			continue
		}
		oldValue, ok := fromDoc[ref.Field]
		entry := ReferencePreview{Reference: ref, OldValue: oldValue}
		if !ok {
			entry.Status = ReferenceBroken
			entry.Detail = fmt.Sprintf("field %q is absent in member %q", ref.Field, ref.From)
			preview.Blockers = append(preview.Blockers, fmt.Sprintf("reference %s.%s -> %s: %s", ref.From, ref.Field, ref.To, entry.Detail))
			refs = append(refs, entry)
			continue
		}
		if owner, found := identityToName[fmt.Sprint(oldValue)]; !found || owner != ref.To {
			entry.Status = ReferenceBroken
			entry.Detail = fmt.Sprintf("value %v does not identify member %q", oldValue, ref.To)
			preview.Blockers = append(preview.Blockers, fmt.Sprintf("reference %s.%s -> %s: %s", ref.From, ref.Field, ref.To, entry.Detail))
			refs = append(refs, entry)
			continue
		}
		migratedFrom := after[ref.From]
		if _, kept := migratedFrom[ref.Field]; !kept {
			entry.Status = ReferenceBroken
			entry.Detail = fmt.Sprintf("field %q was removed from member %q by the migration", ref.Field, ref.From)
			preview.Blockers = append(preview.Blockers, fmt.Sprintf("reference %s.%s -> %s: %s", ref.From, ref.Field, ref.To, entry.Detail))
			refs = append(refs, entry)
			continue
		}
		newIdentity := newIdentities[ref.To]
		if fmt.Sprint(oldValue) == newIdentity {
			entry.Status = ReferenceUnchanged
			entry.NewValue = oldValue
			migratedFrom[ref.Field] = oldValue
		} else {
			entry.Status = ReferenceFollowed
			entry.NewValue = newIdentity
			entry.Detail = fmt.Sprintf("identity of %q changed from %v to %v; reference rewritten", ref.To, oldValue, newIdentity)
			migratedFrom[ref.Field] = newIdentity
		}
		refs = append(refs, entry)
	}
	return refs
}

// ApplyGroup verifies that no member of the group changed since the preview
// was confirmed, then migrates and publishes the whole group atomically.
// Any blocker or any modified input aborts the entire release.
func (e *Engine) ApplyGroup(ctx context.Context, spec GroupSpec, group Group, preview GroupPreview, publish func(context.Context, GroupRelease) error, options ...Option) (GroupResult, error) {
	if len(group) == 0 {
		return GroupResult{}, fmt.Errorf("%w: group is empty", ErrInvalidStep)
	}
	for name, doc := range group {
		from, _ := doc["version"].(string)
		target := preview.Targets[name]
		if target == "" {
			return GroupResult{}, fmt.Errorf("%w: preview does not cover member %q", ErrStalePreview, name)
		}
		if from != target {
			memberPreview, ok := preview.Documents[name]
			if !ok {
				return GroupResult{}, fmt.Errorf("%w: preview does not cover member %q", ErrStalePreview, name)
			}
			if fingerprint(doc, memberPreview.ToVersion, memberPreview.Path) != memberPreview.Fingerprint {
				return GroupResult{}, fmt.Errorf("%w: member %q changed after the group preview was confirmed; run PreviewGroup again", ErrStalePreview, name)
			}
		}
	}
	if groupFingerprint(group, preview.Targets, preview.Documents) != preview.Fingerprint {
		return GroupResult{}, fmt.Errorf("%w: group changed after the preview was confirmed; run PreviewGroup again", ErrStalePreview)
	}
	if len(preview.Blockers) > 0 {
		return GroupResult{}, fmt.Errorf("%w: %v", ErrGroupBlocked, preview.Blockers)
	}

	originals := group.Clone()
	migrated := make(Group, len(group))
	results := make(map[string]Result, len(group))
	records := make(map[string][]Record, len(group))
	var warnings []string
	for _, name := range sortedNames(group) {
		target := preview.Targets[name]
		result, err := e.Migrate(ctx, group[name], target, options...)
		if err != nil {
			return GroupResult{}, fmt.Errorf("member %q: %w", name, err)
		}
		migrated[name] = result.Document
		results[name] = result
		records[name] = result.Records
		for _, warning := range result.Warnings {
			warnings = append(warnings, fmt.Sprintf("member %q: %s", name, warning))
		}
	}

	// Re-apply reference rewriting on the final documents so followed
	// identities are consistent across the released group.
	finalPreview := GroupPreview{Targets: preview.Targets}
	refs := resolveReferences(spec, originals, migrated, &finalPreview)
	if len(finalPreview.Blockers) > 0 {
		return GroupResult{}, fmt.Errorf("%w: %v", ErrGroupBlocked, finalPreview.Blockers)
	}

	release := GroupRelease{
		Originals:          originals,
		Documents:          migrated,
		Targets:            preview.Targets,
		Records:            records,
		References:         refs,
		PreviewFingerprint: preview.Fingerprint,
	}
	if err := publish(ctx, release); err != nil {
		return GroupResult{}, fmt.Errorf("publish group: %w", err)
	}
	return GroupResult{Documents: results, References: refs, Warnings: warnings}, nil
}
