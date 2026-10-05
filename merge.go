package configmigrator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// CandidateSide names one of the two migration candidates, or the common
// source a resolution may fall back to.
type CandidateSide string

const (
	SideA      CandidateSide = "a"
	SideB      CandidateSide = "b"
	SideSource CandidateSide = "source"
)

// OriginLink traces one logical configuration object across the common
// source and both candidates, even when a team renamed or split it. An
// empty field name means the object is absent on that side.
type OriginLink struct {
	Source string `json:"source,omitempty"`
	A      string `json:"a,omitempty"`
	B      string `json:"b,omitempty"`
}

// itemID identifies the logical object regardless of renames: the source
// field when known, otherwise whichever candidate field exists.
func (l OriginLink) itemID() string {
	switch {
	case l.Source != "":
		return l.Source
	case l.A != "":
		return l.A
	default:
		return l.B
	}
}

// MergeItemKind classifies how the two candidates diverged for one logical
// object, relative to the common source.
type MergeItemKind string

const (
	// ItemAgreed means both candidates hold the same value; no decision needed.
	ItemAgreed MergeItemKind = "agreed"
	// ItemAdded means the object exists only in one candidate.
	ItemAdded MergeItemKind = "added"
	// ItemRemoved means the object existed in the source and one candidate dropped it.
	ItemRemoved MergeItemKind = "removed"
	// ItemModified means exactly one candidate changed the source value.
	ItemModified MergeItemKind = "modified"
	// ItemConflict means both candidates changed the same meaning differently
	// (including one side modifying while the other removed it).
	ItemConflict MergeItemKind = "conflict"
)

// MergeItem is one comparable logical object in a merge preview.
type MergeItem struct {
	ID          string        `json:"id"`
	Kind        MergeItemKind `json:"kind"`
	Link        OriginLink    `json:"link"`
	SourceValue any           `json:"source_value,omitempty"`
	ValueA      any           `json:"value_a,omitempty"`
	ValueB      any           `json:"value_b,omitempty"`
	// Fingerprint binds resolutions to this exact three-way state; when a
	// candidate changes, only items whose fingerprint changed need re-checking.
	Fingerprint string `json:"fingerprint"`
}

// MergeReference groups items that must be resolved together and from the
// same side, because they belong to one association (for example a field
// split into two related fields). Adopting a merge while a reference is
// only partially resolved, or resolved from mixed sides, is blocked.
type MergeReference struct {
	Name  string   `json:"name"`
	Items []string `json:"items"`
}

// MergePreview compares both candidates against the common source through
// the origin links. It never mutates its inputs.
type MergePreview struct {
	Items       []MergeItem      `json:"items"`
	References  []MergeReference `json:"references,omitempty"`
	Fingerprint string           `json:"fingerprint"`
	ComputedAt  time.Time        `json:"computed_at"`
}

// Resolution records one human decision for one item, with the reason.
type Resolution struct {
	ItemID string        `json:"item_id"`
	Choice CandidateSide `json:"choice"`
	Reason string        `json:"reason"`
	// ItemFingerprint binds the decision to the item state it was made
	// against; RecheckResolutions uses it to re-check only affected items.
	ItemFingerprint string `json:"item_fingerprint,omitempty"`
}

func hashAny(parts ...any) string {
	payload, _ := json.Marshal(parts)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func lookupField(doc Document, name string) (any, bool) {
	if name == "" {
		return nil, false
	}
	value, ok := doc[name]
	return value, ok
}

// CompareCandidates builds the three-way preview. Every logical object that
// should be correlated across candidates must be covered by an OriginLink,
// so renamed or split objects are compared by meaning, not by name.
func CompareCandidates(source, a, b Document, links []OriginLink, refs []MergeReference) (MergePreview, error) {
	if source == nil || a == nil || b == nil {
		return MergePreview{}, fmt.Errorf("%w: source and both candidates are required", ErrInvalidStep)
	}
	seen := make(map[string]bool)
	preview := MergePreview{ComputedAt: timeNow().UTC()}
	for _, link := range links {
		if link.Source == "" && link.A == "" && link.B == "" {
			return MergePreview{}, fmt.Errorf("%w: origin link names no field on any side", ErrInvalidStep)
		}
		id := link.itemID()
		if seen[id] {
			return MergePreview{}, fmt.Errorf("%w: duplicate origin link for %q", ErrInvalidStep, id)
		}
		seen[id] = true

		sourceValue, hasSource := lookupField(source, link.Source)
		valueA, hasA := lookupField(a, link.A)
		valueB, hasB := lookupField(b, link.B)
		item := MergeItem{
			ID:          id,
			Link:        link,
			Fingerprint: hashAny("item", id, link, hasSource, sourceValue, hasA, valueA, hasB, valueB),
		}
		if hasSource {
			item.SourceValue = cloneValue(sourceValue)
		}
		if hasA {
			item.ValueA = cloneValue(valueA)
		}
		if hasB {
			item.ValueB = cloneValue(valueB)
		}

		aChanged := hasA != hasSource || !reflect.DeepEqual(valueA, sourceValue)
		bChanged := hasB != hasSource || !reflect.DeepEqual(valueB, sourceValue)
		switch {
		case !hasSource && (hasA || hasB):
			item.Kind = ItemAdded
		case hasSource && aChanged && bChanged && !reflect.DeepEqual(valueA, valueB):
			item.Kind = ItemConflict
		case hasSource && aChanged && bChanged:
			item.Kind = ItemAgreed // both sides made the same change
		case hasSource && (!hasA || !hasB):
			item.Kind = ItemRemoved
		case aChanged || bChanged:
			item.Kind = ItemModified
		default:
			item.Kind = ItemAgreed
		}
		preview.Items = append(preview.Items, item)
	}
	sort.Slice(preview.Items, func(i, j int) bool { return preview.Items[i].ID < preview.Items[j].ID })
	preview.References = append([]MergeReference(nil), refs...)
	preview.Fingerprint = hashAny("merge", source, a, b, links, refs)
	return preview, nil
}

// Pending lists the IDs of items that still need a human decision.
func (p MergePreview) Pending() []string {
	var pending []string
	for _, item := range p.Items {
		if item.Kind != ItemAgreed {
			pending = append(pending, item.ID)
		}
	}
	return pending
}

func (p MergePreview) item(id string) *MergeItem {
	for i := range p.Items {
		if p.Items[i].ID == id {
			return &p.Items[i]
		}
	}
	return nil
}

// validateResolutions checks every resolution against the preview: known
// item, matching fingerprint, non-empty reason, and a valid side. It
// returns the IDs of items that still lack a decision.
func validateResolutions(preview MergePreview, resolutions []Resolution) ([]string, error) {
	byItem := make(map[string]Resolution, len(resolutions))
	for _, res := range resolutions {
		item := preview.item(res.ItemID)
		if item == nil {
			return nil, fmt.Errorf("%w: %q", ErrUnknownMergeItem, res.ItemID)
		}
		if res.ItemFingerprint != "" && res.ItemFingerprint != item.Fingerprint {
			return nil, fmt.Errorf("%w: item %q changed after the decision was recorded; re-check it", ErrStalePreview, res.ItemID)
		}
		if res.Reason == "" {
			return nil, fmt.Errorf("%w: decision for item %q has no reason", ErrMergeUnresolved, res.ItemID)
		}
		switch res.Choice {
		case SideA, SideB, SideSource:
		default:
			return nil, fmt.Errorf("%w: decision for item %q names unknown side %q", ErrInvalidStep, res.ItemID, res.Choice)
		}
		byItem[res.ItemID] = res
	}
	var missing []string
	for _, item := range preview.Items {
		if item.Kind == ItemAgreed {
			continue
		}
		if _, ok := byItem[item.ID]; !ok {
			missing = append(missing, item.ID)
		}
	}
	return missing, nil
}

// referenceBlockers lists every association whose member items are not all
// resolved from the same side. Agreed items are ignored because both sides
// hold the same value, and falling back to the source keeps the association.
func referenceBlockers(preview MergePreview, resolutions []Resolution) []string {
	byItem := make(map[string]Resolution, len(resolutions))
	for _, res := range resolutions {
		byItem[res.ItemID] = res
	}
	var blockers []string
	for _, ref := range preview.References {
		side := CandidateSide("")
		var missing []string
		inconsistent := false
		for _, id := range ref.Items {
			item := preview.item(id)
			if item == nil {
				blockers = append(blockers, fmt.Sprintf("reference %q names unknown item %q", ref.Name, id))
				inconsistent = true
				break
			}
			if item.Kind == ItemAgreed {
				continue
			}
			res, ok := byItem[id]
			if !ok {
				missing = append(missing, id)
				continue
			}
			if res.Choice == SideSource {
				continue
			}
			if side == "" {
				side = res.Choice
			} else if res.Choice != side {
				blockers = append(blockers, fmt.Sprintf("reference %q mixes side %q and side %q across its items", ref.Name, side, res.Choice))
				inconsistent = true
				break
			}
		}
		if !inconsistent && len(missing) > 0 {
			blockers = append(blockers, fmt.Sprintf("reference %q is incomplete: items %v still need decisions", ref.Name, missing))
		}
	}
	return blockers
}

// AdoptMerge applies the resolutions and returns the merged document. Every
// non-agreed item must have a reasoned resolution bound to the current item
// state, and every declared reference must be completely and consistently
// resolved; otherwise adoption is refused and nothing is produced.
func AdoptMerge(source Document, preview MergePreview, resolutions []Resolution) (Document, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: source document is required", ErrInvalidStep)
	}
	missing, err := validateResolutions(preview, resolutions)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: items %v still need decisions", ErrMergeUnresolved, missing)
	}
	if blockers := referenceBlockers(preview, resolutions); len(blockers) > 0 {
		return nil, fmt.Errorf("%w: %v", ErrMergeBlocked, blockers)
	}
	byItem := make(map[string]Resolution, len(resolutions))
	for _, res := range resolutions {
		byItem[res.ItemID] = res
	}
	merged := source.Clone()
	if merged == nil {
		merged = Document{}
	}
	put := func(field string, value any, present bool) {
		if field == "" {
			return
		}
		if present {
			merged[field] = cloneValue(value)
		} else {
			delete(merged, field)
		}
	}
	for _, item := range preview.Items {
		if item.Kind == ItemAgreed {
			put(item.Link.A, item.ValueA, item.Link.A != "" && item.ValueA != nil)
			if item.Link.B != "" && item.Link.B != item.Link.A {
				delete(merged, item.Link.B)
			}
			if item.Link.Source != "" && item.Link.Source != item.Link.A {
				delete(merged, item.Link.Source)
			}
			continue
		}
		res := byItem[item.ID]
		switch res.Choice {
		case SideA:
			put(item.Link.A, item.ValueA, item.Link.A != "" && item.ValueA != nil)
			if item.Link.B != "" && item.Link.B != item.Link.A {
				delete(merged, item.Link.B)
			}
		case SideB:
			put(item.Link.B, item.ValueB, item.Link.B != "" && item.ValueB != nil)
			if item.Link.A != "" && item.Link.A != item.Link.B {
				delete(merged, item.Link.A)
			}
		case SideSource:
			put(item.Link.Source, item.SourceValue, item.Link.Source != "" && item.SourceValue != nil)
			if item.Link.A != "" && item.Link.A != item.Link.Source {
				delete(merged, item.Link.A)
			}
			if item.Link.B != "" && item.Link.B != item.Link.Source {
				delete(merged, item.Link.B)
			}
		}
	}
	return merged, nil
}

// RecheckReport states which recorded decisions survive a candidate change
// and which must be reviewed again.
type RecheckReport struct {
	// Valid are item IDs whose decisions still match the recomputed items.
	Valid []string `json:"valid"`
	// Stale are item IDs whose items changed, vanished, or no longer need a
	// decision; only these must be re-made. All other decisions stand.
	Stale []string `json:"stale"`
	// NewItems are item IDs that did not exist in the previous preview.
	NewItems []string `json:"new_items,omitempty"`
}

// RecheckResolutions recompares the (possibly updated) candidates against
// the same source, links, and references, and reports exactly which
// resolutions are affected. Unchanged items keep their decisions.
func RecheckResolutions(source, a, b Document, links []OriginLink, refs []MergeReference, previous MergePreview, resolutions []Resolution) (MergePreview, RecheckReport, error) {
	next, err := CompareCandidates(source, a, b, links, refs)
	if err != nil {
		return MergePreview{}, RecheckReport{}, err
	}
	report := RecheckReport{}
	for _, res := range resolutions {
		item := next.item(res.ItemID)
		switch {
		case item == nil:
			report.Stale = append(report.Stale, res.ItemID)
		case item.Fingerprint != res.ItemFingerprint:
			report.Stale = append(report.Stale, res.ItemID)
		case item.Kind == ItemAgreed:
			report.Stale = append(report.Stale, res.ItemID) // decision no longer needed
		default:
			report.Valid = append(report.Valid, res.ItemID)
		}
	}
	for _, item := range next.Items {
		if previous.item(item.ID) == nil {
			report.NewItems = append(report.NewItems, item.ID)
		}
	}
	sort.Strings(report.Valid)
	sort.Strings(report.Stale)
	sort.Strings(report.NewItems)
	return next, report, nil
}
