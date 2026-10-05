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

// MergeChoice selects which side of a merge item is adopted.
type MergeChoice string

const (
	ChoiceA    MergeChoice = "a"
	ChoiceB    MergeChoice = "b"
	ChoiceBase MergeChoice = "base"
)

// MergeItemKind classifies how the two candidates relate for one correlated
// meaning (a base field, or a candidate-only field).
type MergeItemKind string

const (
	// ItemUnchanged means both candidates kept the base value.
	ItemUnchanged MergeItemKind = "unchanged"
	// ItemAdded means exactly one side introduced a new field, or both
	// introduced it with the same value.
	ItemAdded MergeItemKind = "added"
	// ItemRemoved means exactly one side dropped the base field, or both did.
	ItemRemoved MergeItemKind = "removed"
	// ItemModified means exactly one side changed the base value, or both
	// changed it to the same new value.
	ItemModified MergeItemKind = "modified"
	// ItemConflict means both sides changed the same meaning differently.
	ItemConflict MergeItemKind = "conflict"
)

// MergeOptions declares how candidate fields trace back to the common
// source. OriginA and OriginB map a candidate field to the base field it
// derives from; several candidate fields may share one base field (split),
// and a renamed field simply points at its old name. Fields without an
// entry correlate by name.
type MergeOptions struct {
	OriginA    map[string]string `json:"origin_a,omitempty"`
	OriginB    map[string]string `json:"origin_b,omitempty"`
	References []MergeReference  `json:"references,omitempty"`
}

// MergeReference declares that two fields of the merged document must hold
// the same value. Adoption is blocked while any reference is incomplete.
type MergeReference struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

// MergeReferenceOutcome reports whether one reference holds in the merged
// document produced by the current decisions.
type MergeReferenceOutcome struct {
	Reference MergeReference `json:"reference"`
	Resolved  bool           `json:"resolved"`
	Detail    string         `json:"detail,omitempty"`
}

// MergeItem correlates one meaning across base and both candidates. When a
// side split the base field into several, its value is a Document of the
// contributing fields; otherwise it is the plain field value.
type MergeItem struct {
	ID          string        `json:"id"`
	Kind        MergeItemKind `json:"kind"`
	Base        any           `json:"base,omitempty"`
	A           any           `json:"a,omitempty"`
	B           any           `json:"b,omitempty"`
	FieldsA     []string      `json:"fields_a,omitempty"`
	FieldsB     []string      `json:"fields_b,omitempty"`
	Fingerprint string        `json:"fingerprint"`
}

// MergePlan is the result of correlating two candidates against their
// common source. Its fingerprint binds it to the exact inputs, so a plan
// computed before a candidate changed cannot be adopted silently.
type MergePlan struct {
	Items       []MergeItem `json:"items"`
	Fingerprint string      `json:"fingerprint"`
	ComputedAt  time.Time   `json:"computed_at"`
}

// MergeDecision records a human choice for one item together with the
// reason. Conflict items may only be adopted with a non-empty rationale.
type MergeDecision struct {
	Item      string      `json:"item"`
	Choice    MergeChoice `json:"choice"`
	Rationale string      `json:"rationale,omitempty"`
}

// MergeResolution is the outcome of applying decisions to a plan.
type MergeResolution struct {
	Document   Document                `json:"document"`
	Unresolved []string                `json:"unresolved,omitempty"`
	References []MergeReferenceOutcome `json:"references,omitempty"`
	Blockers   []string                `json:"blockers,omitempty"`
	Applied    []MergeDecision         `json:"applied,omitempty"`
}

// MergeRelease is handed to the publisher only when every conflict is
// resolved with a rationale and every reference is complete.
type MergeRelease struct {
	Base      Document        `json:"base"`
	A         Document        `json:"a"`
	B         Document        `json:"b"`
	Decisions []MergeDecision `json:"decisions"`
	Document  Document        `json:"document"`
	Plan      MergePlan       `json:"plan"`
}

func mergeItemFingerprint(base, a, b any) string {
	payload, _ := json.Marshal(map[string]any{"base": base, "a": a, "b": b})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func mergePlanFingerprint(base, a, b Document, opts MergeOptions) string {
	payload, _ := json.Marshal(map[string]any{
		"base":       base,
		"a":          a,
		"b":          b,
		"origin_a":   opts.OriginA,
		"origin_b":   opts.OriginB,
		"references": opts.References,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// candidateView groups a candidate's fields by correlation key: the origin
// base field when declared, otherwise the field name itself. A split base
// field therefore appears once, with all contributing fields collected.
type candidateView struct {
	keys   map[string][]string // correlation key -> candidate fields
	values map[string]any      // correlation key -> value or Document of fields
}

func viewOf(candidate Document, origin map[string]string) candidateView {
	view := candidateView{keys: make(map[string][]string), values: make(map[string]any)}
	for field := range candidate {
		key := field
		if base, ok := origin[field]; ok && base != "" {
			key = base
		}
		view.keys[key] = append(view.keys[key], field)
	}
	for key, fields := range view.keys {
		sort.Strings(fields)
		if len(fields) == 1 {
			view.values[key] = candidate[fields[0]]
			continue
		}
		combined := make(Document, len(fields))
		for _, field := range fields {
			combined[field] = candidate[field]
		}
		view.values[key] = combined
	}
	return view
}

// CompareMerge correlates two candidates that migrated from the same source
// along different paths. Fields renamed or split are matched through the
// declared origins, so the same meaning is compared side by side and
// classified as added, removed, modified, or conflicting.
func CompareMerge(base, a, b Document, opts MergeOptions) (MergePlan, error) {
	if base == nil || a == nil || b == nil {
		return MergePlan{}, fmt.Errorf("%w: base and both candidates are required", ErrInvalidStep)
	}
	viewA := viewOf(a, opts.OriginA)
	viewB := viewOf(b, opts.OriginB)

	keySet := make(map[string]bool)
	for key := range base {
		keySet[key] = true
	}
	for key := range viewA.values {
		keySet[key] = true
	}
	for key := range viewB.values {
		keySet[key] = true
	}
	// The version field is migration metadata maintained by the engine, not
	// part of the configuration meaning the two teams edited.
	delete(keySet, "version")
	keys := make([]string, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	plan := MergePlan{
		Fingerprint: mergePlanFingerprint(base, a, b, opts),
		ComputedAt:  timeNow().UTC(),
	}
	for _, key := range keys {
		baseValue, inBase := base[key]
		valueA, inA := viewA.values[key]
		valueB, inB := viewB.values[key]
		item := MergeItem{
			ID:          key,
			FieldsA:     viewA.keys[key],
			FieldsB:     viewB.keys[key],
			Fingerprint: mergeItemFingerprint(baseValue, valueA, valueB),
		}
		if inBase {
			item.Base = baseValue
		}
		if inA {
			item.A = valueA
		}
		if inB {
			item.B = valueB
		}
		sameAB := inA && inB && reflect.DeepEqual(valueA, valueB)
		switch {
		case !inBase:
			item.Kind = ItemAdded
			if inA && inB && !sameAB {
				item.Kind = ItemConflict
			}
		case !inA && !inB:
			item.Kind = ItemRemoved
		case !inA || !inB:
			item.Kind = ItemRemoved
			present := valueB
			if inA {
				present = valueA
			}
			if !reflect.DeepEqual(baseValue, present) {
				item.Kind = ItemConflict
			}
		default:
			aChanged := sideChanged(baseValue, key, viewA.keys[key], valueA)
			bChanged := sideChanged(baseValue, key, viewB.keys[key], valueB)
			switch {
			case aChanged && bChanged && !sameAB:
				item.Kind = ItemConflict
			case aChanged || bChanged:
				item.Kind = ItemModified
			default:
				item.Kind = ItemUnchanged
			}
		}
		plan.Items = append(plan.Items, item)
	}
	return plan, nil
}

// sideChanged reports whether a side diverges from the base for one
// correlation key, either by value or by field identity (rename or split).
func sideChanged(baseValue any, key string, fields []string, value any) bool {
	if !reflect.DeepEqual(baseValue, value) {
		return true
	}
	return len(fields) != 1 || fields[0] != key
}

func findItem(plan MergePlan, id string) *MergeItem {
	for i := range plan.Items {
		if plan.Items[i].ID == id {
			return &plan.Items[i]
		}
	}
	return nil
}

// ResolveMerge applies decisions to a plan and builds the merged document.
// Non-conflicting items default to the changed side; conflicts stay
// unresolved until a decision with a rationale covers them. References are
// evaluated against the merged document and incomplete ones are reported as
// blockers.
func ResolveMerge(plan MergePlan, decisions []MergeDecision, opts MergeOptions) (MergeResolution, error) {
	byItem := make(map[string]MergeDecision, len(decisions))
	for _, decision := range decisions {
		if decision.Choice != ChoiceA && decision.Choice != ChoiceB && decision.Choice != ChoiceBase {
			return MergeResolution{}, fmt.Errorf("%w: item %q has unknown choice %q", ErrInvalidStep, decision.Item, decision.Choice)
		}
		if findItem(plan, decision.Item) == nil {
			return MergeResolution{}, fmt.Errorf("%w: decision refers to unknown item %q", ErrInvalidStep, decision.Item)
		}
		byItem[decision.Item] = decision
	}

	merged := Document{}
	resolution := MergeResolution{Document: merged}
	for _, item := range plan.Items {
		decision, decided := byItem[item.ID]
		choice := ChoiceBase
		auto := true
		if decided {
			choice = decision.Choice
			auto = false
		} else {
			switch item.Kind {
			case ItemUnchanged:
				choice = ChoiceBase
			case ItemRemoved:
				// Follow the side that dropped the field; an explicit
				// decision for ChoiceBase is required to keep it.
				if item.A == nil {
					choice = ChoiceA
				} else {
					choice = ChoiceB
				}
			case ItemAdded, ItemModified:
				// Follow the side that changed; when both changed to the
				// same value either side works.
				aChanged := sideChanged(item.Base, item.ID, item.FieldsA, item.A)
				bChanged := sideChanged(item.Base, item.ID, item.FieldsB, item.B)
				switch {
				case aChanged && !bChanged:
					choice = ChoiceA
				case bChanged && !aChanged:
					choice = ChoiceB
				case item.A != nil:
					choice = ChoiceA
				default:
					choice = ChoiceB
				}
			case ItemConflict:
				resolution.Unresolved = append(resolution.Unresolved, item.ID)
				continue
			}
		}
		if !auto {
			resolution.Applied = append(resolution.Applied, decision)
		}
		if item.Kind == ItemConflict && decision.Rationale == "" {
			resolution.Unresolved = append(resolution.Unresolved, item.ID)
			continue
		}
		applyChoice(merged, item, choice)
	}

	for _, ref := range opts.References {
		outcome := MergeReferenceOutcome{Reference: ref}
		from, fromOK := merged[ref.From]
		to, toOK := merged[ref.To]
		switch {
		case !fromOK:
			outcome.Detail = fmt.Sprintf("%s: field %q is missing from the merged document", ref.Name, ref.From)
		case !toOK:
			outcome.Detail = fmt.Sprintf("%s: field %q is missing from the merged document", ref.Name, ref.To)
		case !reflect.DeepEqual(from, to):
			outcome.Detail = fmt.Sprintf("%s: %q=%v does not match %q=%v", ref.Name, ref.From, from, ref.To, to)
		default:
			outcome.Resolved = true
		}
		if !outcome.Resolved {
			resolution.Blockers = append(resolution.Blockers, "incomplete reference: "+outcome.Detail)
		}
		resolution.References = append(resolution.References, outcome)
	}
	sort.Strings(resolution.Unresolved)
	sort.Strings(resolution.Blockers)
	return resolution, nil
}

// applyChoice writes the chosen side of an item into the merged document.
// Split items contribute each of their fields; base choice restores the
// original field under its base name.
func applyChoice(merged Document, item MergeItem, choice MergeChoice) {
	switch choice {
	case ChoiceA:
		writeSide(merged, item.ID, item.FieldsA, item.A)
	case ChoiceB:
		writeSide(merged, item.ID, item.FieldsB, item.B)
	case ChoiceBase:
		if item.Base != nil {
			merged[item.ID] = cloneValue(item.Base)
		}
	}
}

func writeSide(merged Document, key string, fields []string, value any) {
	if value == nil {
		return
	}
	if combined, ok := value.(Document); ok && len(fields) > 1 {
		for _, field := range fields {
			merged[field] = cloneValue(combined[field])
		}
		return
	}
	name := key
	if len(fields) == 1 {
		name = fields[0]
	}
	merged[name] = cloneValue(value)
}

// RecheckDecisions revalidates earlier decisions against a freshly computed
// plan. Decisions whose item is unchanged carry over; only decisions on
// items whose fingerprint changed (or that disappeared) are returned as
// stale and must be reviewed again.
func RecheckDecisions(oldPlan MergePlan, decisions []MergeDecision, newPlan MergePlan) (kept, stale []MergeDecision) {
	for _, decision := range decisions {
		oldItem := findItem(oldPlan, decision.Item)
		newItem := findItem(newPlan, decision.Item)
		if oldItem == nil || newItem == nil || oldItem.Fingerprint != newItem.Fingerprint {
			stale = append(stale, decision)
			continue
		}
		kept = append(kept, decision)
	}
	return kept, stale
}

// AdoptMerge formally adopts a merge: every conflict must be resolved with
// a rationale, every declared reference must be complete, and the plan must
// still match the inputs. Only then is the publisher invoked with the
// original candidates and the recorded decisions.
func AdoptMerge(ctx context.Context, base, a, b Document, opts MergeOptions, plan MergePlan, decisions []MergeDecision, publish func(context.Context, MergeRelease) error) (MergeResolution, error) {
	if plan.Fingerprint != mergePlanFingerprint(base, a, b, opts) {
		return MergeResolution{}, fmt.Errorf("%w: a candidate changed after the merge plan was computed; run CompareMerge again", ErrStalePreview)
	}
	resolution, err := ResolveMerge(plan, decisions, opts)
	if err != nil {
		return MergeResolution{}, err
	}
	var blockers []string
	for _, id := range resolution.Unresolved {
		blockers = append(blockers, fmt.Sprintf("item %q is a conflict and needs a decision with a rationale", id))
	}
	blockers = append(blockers, resolution.Blockers...)
	if len(blockers) > 0 {
		return MergeResolution{}, fmt.Errorf("%w: %s", ErrMergeBlocked, strings.Join(blockers, "; "))
	}
	if publish != nil {
		release := MergeRelease{
			Base:      base.Clone(),
			A:         a.Clone(),
			B:         b.Clone(),
			Decisions: append([]MergeDecision(nil), decisions...),
			Document:  resolution.Document.Clone(),
			Plan:      plan,
		}
		if err := publish(ctx, release); err != nil {
			return MergeResolution{}, fmt.Errorf("publish merge: %w", err)
		}
	}
	return resolution, nil
}
