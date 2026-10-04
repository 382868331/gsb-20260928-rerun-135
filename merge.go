package configmigrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// MergeSide identifies one of the two teams that diverged from the same
// common origin, or the shared base itself.
type MergeSide string

const (
	SideBase MergeSide = "base"
	SideA    MergeSide = "a"
	SideB    MergeSide = "b"
)

// MergeCandidate is one side of a merge: the document the team currently
// has, the version it came from on the common base, and the migration path
// that turned the base into this candidate. An empty path means the team
// never migrated.
type MergeCandidate struct {
	Document Document `json:"document"`
	From     string   `json:"from_version,omitempty"`
	Path     []string `json:"path,omitempty"`
	Labels   []string `json:"labels,omitempty"`
}

// MergeRequest binds the two candidates to their exact common origin.
type MergeRequest struct {
	Base Document       `json:"base"`
	A    MergeCandidate `json:"a"`
	B    MergeCandidate `json:"b"`
}

// MergeRef declares that two fields must carry the same identity in the
// merged document (for example a service and the upstream pointing at it).
// Endpoints are named per candidate because different migration paths can
// rename the same object differently.
type MergeRef struct {
	Name  string    `json:"name"`
	FromA FileField `json:"from_a"`
	ToA   FileField `json:"to_a"`
	FromB FileField `json:"from_b"`
	ToB   FileField `json:"to_b"`
}

// MergeOrigin describes one provenance fact for a merged field: the field
// existed at this stage under this name.
type MergeOrigin struct {
	Side  MergeSide `json:"side"`
	File  string    `json:"file"`
	Field string    `json:"field"`
	Step  string    `json:"step,omitempty"`
	Split bool      `json:"split,omitempty"`
}

// MergeChoiceStatus tells whether one conflict item is still open.
type MergeChoiceStatus string

const (
	MergeAuto       MergeChoiceStatus = "auto"
	MergeResolved   MergeChoiceStatus = "resolved"
	MergeUnresolved MergeChoiceStatus = "unresolved"
	MergeStale      MergeChoiceStatus = "stale"
)

// MergeItem is one logical object/field in the merge. Fields whose lineage
// traces back to the same base origin are always one item, even when the
// two teams migrated them to different names.
type MergeItem struct {
	ID        string                       `json:"id"`
	Origins   []MergeOrigin                `json:"origins"`
	Fields    map[MergeSide][]string       `json:"fields,omitempty"`
	BaseValue any                          `json:"base_value,omitempty"`
	Values    map[MergeSide]map[string]any `json:"values,omitempty"`
	Changed   map[MergeSide]bool           `json:"changed,omitempty"`
	Split     bool                         `json:"split,omitempty"`
	// Kind is add / delete / modify / equal / split.
	Kind string `json:"kind"`
	// Options are the selectable resolutions: "a", "b", "base", "drop",
	// "keep", or "custom".
	Options     []string          `json:"options,omitempty"`
	Status      MergeChoiceStatus `json:"status"`
	Choice      string            `json:"choice,omitempty"`
	CustomValue any               `json:"custom_value,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	RelatedIDs  []string          `json:"related_ids,omitempty"`
	StaleReason string            `json:"stale_reason,omitempty"`
}

// MergeRefOutcome shows how one cross-reference resolves on each candidate
// and in the current merged draft.
type MergeRefOutcome struct {
	Ref        MergeRef `json:"ref"`
	AOK        bool     `json:"a_ok"`
	BOK        bool     `json:"b_ok"`
	MergedOK   bool     `json:"merged_ok"`
	AFrom      string   `json:"a_from,omitempty"`
	ATo        string   `json:"a_to,omitempty"`
	BFrom      string   `json:"b_from,omitempty"`
	BTo        string   `json:"b_to,omitempty"`
	MergedFrom string   `json:"merged_from,omitempty"`
	MergedTo   string   `json:"merged_to,omitempty"`
	RelatedIDs []string `json:"related_ids,omitempty"`
	Problem    string   `json:"problem,omitempty"`
}

// MergePreview is the complete, itemized picture of a merge. It never hides
// an unresolved item: Blockers lists every reason the draft cannot become
// the official version.
type MergePreview struct {
	Items       []MergeItem            `json:"items"`
	Refs        []MergeRefOutcome      `json:"refs,omitempty"`
	Paths       map[MergeSide][]string `json:"paths,omitempty"`
	Blockers    []string               `json:"blockers,omitempty"`
	Fingerprint string                 `json:"fingerprint"`
}

func mergeFingerprint(req MergeRequest, refs []MergeRef) string {
	payload, _ := json.Marshal(map[string]any{"request": req, "refs": refs})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// lineageNode is one field occurrence produced at a migration stage.
type lineageNode struct {
	side  MergeSide
	field string
	step  string
	split bool
}

type lineage struct {
	origins map[string][]lineageNode
}

// lineageFor runs the candidate's documented path on a private copy and
// records, per final field, every base/stage field it was derived from.
func (e *Engine) lineageFor(ctx context.Context, start Document, from string, path []string, side MergeSide) (Document, lineage, error) {
	lin := lineage{origins: map[string][]lineageNode{}}
	if len(path) == 0 {
		out := start.Clone()
		for field := range out {
			if field == "version" {
				continue
			}
			lin.origins[field] = []lineageNode{{side: SideBase, field: field}}
		}
		return out, lin, nil
	}
	if path[0] != from {
		return nil, lineage{}, fmt.Errorf("%w: candidate path must start at base version %q", ErrInvalidStep, from)
	}
	steps, err := e.Plan(from, path[len(path)-1], WithPath(path...))
	if err != nil {
		return nil, lineage{}, err
	}
	// active[field] is the current provenance chain for this live field.
	active := map[string][]lineageNode{}
	for field := range start {
		if field == "version" {
			continue
		}
		active[field] = []lineageNode{{side: SideBase, field: field}}
	}
	working := start.Clone()
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return nil, lineage{}, err
		}
		before := working.Clone()
		next, err := runStep(ctx, step, working)
		if err != nil {
			return nil, lineage{}, fmt.Errorf("migration %s -> %s failed: %w", step.From, step.To, err)
		}
		if next == nil {
			return nil, lineage{}, fmt.Errorf("migration %s -> %s returned nil", step.From, step.To)
		}
		declared := map[string]FieldEffect{}
		for _, effect := range step.Effects {
			declared[effect.Field] = effect
		}
		label := step.From + " -> " + step.To
		derived := map[string][]lineageNode{}
		for field, afterValue := range next {
			if field == "version" {
				continue
			}
			var chain []lineageNode
			node := lineageNode{side: side, field: field, step: label}
			if effect, ok := declared[field]; ok && len(effect.From) > 0 {
				sources := append([]string(nil), effect.From...)
				sort.Strings(sources)
				for _, src := range effect.From {
					if prior, exists := active[src]; exists {
						chain = append(chain, prior...)
					} else {
						chain = append(chain, lineageNode{side: SideBase, field: src})
					}
				}
				node.split = len(sources) >= 2
			} else if beforeValue, existed := before[field]; existed && reflect.DeepEqual(beforeValue, afterValue) {
				chain = append(chain, active[field]...)
			}
			// A field that changed without a declared origin is derived from
			// its same-named predecessor whenever one existed.
			if len(chain) == 0 {
				if _, existed := before[field]; existed {
					chain = append(chain, active[field]...)
				}
			}
			chain = append(chain, node)
			derived[field] = chain
		}
		active = derived
		working = next
	}
	lin.origins = active
	return working, lin, nil
}

// alignedEntry is one side's occurrence of a logical object.
type alignedEntry struct {
	side  MergeSide
	field string
	value any
	chain []lineageNode
}

type alignedCluster struct {
	entries []alignedEntry
	split   bool
}

func baseKeys(chain []lineageNode) []string {
	seen := map[string]bool{}
	var keys []string
	for _, node := range chain {
		if node.side == SideBase && !seen[node.field] {
			seen[node.field] = true
			keys = append(keys, node.field)
		}
	}
	sort.Strings(keys)
	return keys
}

// alignClusters groups field occurrences by their common base origin. Two
// candidates that renamed or split the same base field stay one object
// instead of appearing twice. Fields with no base origin are matched by
// equal name between the two candidates; genuinely one-sided additions
// stay their own cluster.
func alignClusters(base Document, a Document, la lineage, b Document, lb lineage) []alignedCluster {
	type entry = alignedEntry
	var entries []entry
	for field := range base {
		if field == "version" {
			continue
		}
		entries = append(entries, entry{side: SideBase, field: field, value: base[field],
			chain: []lineageNode{{side: SideBase, field: field}}})
	}
	addSide := func(doc Document, lin lineage, side MergeSide) {
		names := make([]string, 0, len(doc))
		for field := range doc {
			if field != "version" {
				names = append(names, field)
			}
		}
		sort.Strings(names)
		for _, field := range names {
			entries = append(entries, entry{side: side, field: field, value: doc[field], chain: lin.origins[field]})
		}
	}
	addSide(a, la, SideA)
	addSide(b, lb, SideB)

	parent := make([]int, len(entries))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(i, j int) {
		ri, rj := find(i), find(j)
		if ri != rj {
			parent[rj] = ri
		}
	}

	// Union entries that share any base origin key.
	byBase := map[string][]int{}
	for i, en := range entries {
		for _, key := range baseKeys(en.chain) {
			byBase[key] = append(byBase[key], i)
		}
	}
	for _, ids := range byBase {
		for _, id := range ids[1:] {
			union(ids[0], id)
		}
	}

	// Name-align entries that have no base origin at all.
	nameA := map[string]int{}
	nameB := map[string]int{}
	for i, en := range entries {
		if len(baseKeys(en.chain)) > 0 {
			continue
		}
		if en.side == SideA {
			nameA[en.field] = i
		} else if en.side == SideB {
			nameB[en.field] = i
		}
	}
	for name, ia := range nameA {
		if ib, ok := nameB[name]; ok {
			union(ia, ib)
		}
	}

	groups := map[int][]int{}
	for i := range entries {
		r := find(i)
		groups[r] = append(groups[r], i)
	}
	out := make([]alignedCluster, 0, len(groups))
	for _, ids := range groups {
		cluster := alignedCluster{}
		seen := map[string]bool{}
		for _, id := range ids {
			en := entries[id]
			key := string(en.side) + ":" + en.field
			if seen[key] {
				continue
			}
			seen[key] = true
			cluster.entries = append(cluster.entries, en)
			for _, node := range en.chain {
				if node.split {
					cluster.split = true
				}
			}
		}
		// A base origin that reaches several live fields on one side is a
		// split even when the registered effects did not enumerate it.
		perSide := map[MergeSide]map[string]bool{}
		for _, en := range cluster.entries {
			if en.side == SideBase {
				continue
			}
			if perSide[en.side] == nil {
				perSide[en.side] = map[string]bool{}
			}
			for _, key := range baseKeys(en.chain) {
				if perSide[en.side][key] {
					cluster.split = true
				}
				perSide[en.side][key] = true
			}
		}
		sort.Slice(cluster.entries, func(i, j int) bool {
			if cluster.entries[i].side != cluster.entries[j].side {
				return cluster.entries[i].side < cluster.entries[j].side
			}
			return cluster.entries[i].field < cluster.entries[j].field
		})
		out = append(out, cluster)
	}
	sort.Slice(out, func(i, j int) bool {
		ki := clusterBaseKeys(out[i])
		kj := clusterBaseKeys(out[j])
		if len(ki) > 0 || len(kj) > 0 {
			if len(ki) == 0 {
				return false
			}
			if len(kj) == 0 {
				return true
			}
			if ki[0] != kj[0] {
				return ki[0] < kj[0]
			}
		}
		return out[i].entries[0].field < out[j].entries[0].field
	})
	return out
}

func clusterBaseKeys(c alignedCluster) []string {
	seen := map[string]bool{}
	var keys []string
	for _, en := range c.entries {
		for _, key := range baseKeys(en.chain) {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}
