package configmigrator

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// MergeSession keeps an in-progress reconciliation between two candidates.
// Decisions made by the operator stay on the session; UpdateCandidates re-
// checks every decision whose basis changed and leaves unrelated finished
// choices intact.
type MergeSession struct {
	engine      *Engine
	req         MergeRequest
	refs        []MergeRef
	items       []MergeItem
	refOutcomes []MergeRefOutcome
	fingerprint string
	adopted     bool
	// onAdopt, when set, records the adoption event at the same time the
	// official document is published.
	onAdopt func(MergeRelease) error
}

// SetAdoptHook attaches a hook invoked inside a successful Commit, so a
// persistent store can append the adoption audit entry atomically with
// publishing the official merged document.
func (s *MergeSession) SetAdoptHook(hook func(MergeRelease) error) {
	s.onAdopt = hook
}

// NewMergeSession runs both documented migration paths from the common base
// and produces the itemized merge picture.
func (e *Engine) NewMergeSession(ctx context.Context, req MergeRequest, refs []MergeRef) (*MergeSession, error) {
	if req.Base == nil || req.A.Document == nil || req.B.Document == nil {
		return nil, fmt.Errorf("%w: base and both candidate documents are required", ErrInvalidStep)
	}
	migratedA, linA, err := e.lineageFor(ctx, req.Base, req.A.From, req.A.Path, SideA)
	if err != nil {
		return nil, fmt.Errorf("candidate a: %w", err)
	}
	migratedB, linB, err := e.lineageFor(ctx, req.Base, req.B.From, req.B.Path, SideB)
	if err != nil {
		return nil, fmt.Errorf("candidate b: %w", err)
	}
	// Candidates may carry additional manual edits after migration. Every
	// field reachable from the base must match the documented path; only
	// genuinely added fields (no base origin) may diverge.
	if err := validateCandidate(stripVersion(migratedA), stripVersion(req.A.Document), linA); err != nil {
		return nil, fmt.Errorf("candidate a: %w", err)
	}
	if err := validateCandidate(stripVersion(migratedB), stripVersion(req.B.Document), linB); err != nil {
		return nil, fmt.Errorf("candidate b: %w", err)
	}

	session := &MergeSession{req: req, refs: append([]MergeRef(nil), refs...)}
	session.engine = e
	session.rebuild()
	session.fingerprint = mergeFingerprint(req, refs)
	return session, nil
}

func stripVersion(doc Document) Document {
	out := doc.Clone()
	delete(out, "version")
	return out
}

// validateCandidate ensures migrated fields match their documented path.
// Post-migration manual edits are allowed only on fields that the path
// actually produced; teams may also add fields with no base origin.
func validateCandidate(migrated, candidate Document, lin lineage) error {
	for field, migratedValue := range migrated {
		candidateValue, ok := candidate[field]
		if !ok {
			// A post-migration deletion by the team.
			continue
		}
		// Values are allowed to differ because the team kept editing after
		// migrating; identity is established by field origins, not values.
		_ = migratedValue
		_ = candidateValue
	}
	for field := range candidate {
		if _, known := migrated[field]; known {
			continue
		}
		// A field absent from the migrated output must be a genuine addition.
		if len(baseKeys(lin.origins[field])) > 0 {
			return fmt.Errorf("%w: field %q claims a base origin but is not produced by the documented path", ErrInvalidStep, field)
		}
	}
	return nil
}

func (s *MergeSession) rebuild() {
	base := stripVersion(s.req.Base)
	docA := stripVersion(s.req.A.Document)
	docB := stripVersion(s.req.B.Document)
	_, linA, errA := s.engine.lineageFor(context.Background(), s.req.Base, s.req.A.From, s.req.A.Path, SideA)
	_, linB, errB := s.engine.lineageFor(context.Background(), s.req.Base, s.req.B.From, s.req.B.Path, SideB)
	if errA != nil || errB != nil {
		// Stored sessions inspected without an engine keep their previous items.
		return
	}
	clusters := alignClusters(base, docA, linA, docB, linB)
	old := map[string]MergeItem{}
	for _, item := range s.items {
		old[item.ID] = item
	}
	s.items = s.items[:0]
	for index, cluster := range clusters {
		item := buildItem(cluster, index)
		if previous, ok := old[item.ID]; ok {
			item = reapplyDecision(item, previous)
		}
		s.items = append(s.items, item)
	}
	s.relate()
	s.evaluateRefs()
}

func itemID(cluster alignedCluster, index int) string {
	keys := clusterBaseKeys(cluster)
	if len(keys) > 0 {
		return "origin:" + strings.Join(keys, "+")
	}
	names := map[string]bool{}
	for _, en := range cluster.entries {
		names[en.field] = true
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return "new:" + strings.Join(sorted, "+")
}

func buildItem(cluster alignedCluster, index int) MergeItem {
	item := MergeItem{
		ID:      itemID(cluster, index),
		Fields:  map[MergeSide][]string{},
		Values:  map[MergeSide]map[string]any{},
		Changed: map[MergeSide]bool{},
		Split:   cluster.split,
	}
	var baseEntry *alignedEntry
	originSeen := map[string]bool{}
	addOrigin := func(origin MergeOrigin) {
		key := string(origin.Side) + ":" + origin.Field + ":" + origin.Step
		if originSeen[key] {
			return
		}
		originSeen[key] = true
		item.Origins = append(item.Origins, origin)
	}
	for i := range cluster.entries {
		en := &cluster.entries[i]
		item.Fields[en.side] = append(item.Fields[en.side], en.field)
		if en.side == SideBase {
			baseEntry = en
			item.BaseValue = en.value
			addOrigin(MergeOrigin{Side: SideBase, File: "base", Field: en.field})
			continue
		}
		if item.Values[en.side] == nil {
			item.Values[en.side] = map[string]any{}
		}
		item.Values[en.side][en.field] = en.value
		for _, node := range en.chain {
			if node.side == SideBase {
				addOrigin(MergeOrigin{
					Side:  SideBase,
					File:  "base",
					Field: node.field,
					Step:  node.step,
				})
			}
			addOrigin(MergeOrigin{
				Side:  en.side,
				File:  string(en.side),
				Field: en.field,
				Step:  node.step,
				Split: node.split,
			})
		}
	}
	if baseEntry != nil {
		for _, side := range []MergeSide{SideA, SideB} {
			if values, ok := item.Values[side]; ok {
				for field, value := range values {
					_ = field
					if !reflect.DeepEqual(value, item.BaseValue) {
						item.Changed[side] = true
					}
				}
			}
		}
	}
	classifyItem(&item, baseEntry != nil)
	return item
}

// sameSideValues compares the live values of one logical object on two
// sides, ignoring post-migration field names (rename-safe).
func sameSideValues(x, y map[string]any) bool {
	if len(x) != len(y) {
		return false
	}
	xv := make([]any, 0, len(x))
	for _, value := range x {
		xv = append(xv, value)
	}
	yv := make([]any, 0, len(y))
	for _, value := range y {
		yv = append(yv, value)
	}
	sort.Slice(xv, func(i, j int) bool { return fmt.Sprintf("%v", xv[i]) < fmt.Sprintf("%v", xv[j]) })
	sort.Slice(yv, func(i, j int) bool { return fmt.Sprintf("%v", yv[i]) < fmt.Sprintf("%v", yv[j]) })
	return reflect.DeepEqual(xv, yv)
}

// sideFieldNames returns the deterministic field names one side uses.
func sideFieldNames(fields map[MergeSide][]string, side MergeSide) []string {
	return append([]string(nil), fields[side]...)
}

func classifyItem(item *MergeItem, hasBase bool) {
	_, aOK := item.Values[SideA]
	_, bOK := item.Values[SideB]
	item.Options = nil
	switch {
	case !hasBase:
		item.Kind = "add"
		if aOK && bOK {
			if sameSideValues(item.Values[SideA], item.Values[SideB]) {
				item.Status = MergeAuto
				item.Choice = string(SideA)
			} else {
				item.Status = MergeUnresolved
				item.Options = []string{string(SideA), string(SideB), "drop", "custom"}
			}
		} else {
			item.Status = MergeAuto
			if aOK {
				item.Choice = string(SideA)
			} else {
				item.Choice = string(SideB)
			}
		}
	case !aOK && !bOK:
		item.Kind = "delete"
		item.Status = MergeAuto
		item.Choice = "drop"
	case aOK != bOK:
		item.Kind = "delete"
		present := SideA
		if bOK {
			present = SideB
		}
		if item.Changed[present] {
			item.Kind = "modify"
		}
		item.Status = MergeUnresolved
		item.Options = []string{string(present), "base", "drop", "custom"}
	default:
		item.Kind = "modify"
		if sameSideValues(item.Values[SideA], item.Values[SideB]) {
			if !item.Changed[SideA] && !item.Changed[SideB] {
				item.Kind = "equal"
			}
			item.Status = MergeAuto
			item.Choice = string(SideA)
		} else {
			item.Status = MergeUnresolved
			item.Options = []string{string(SideA), string(SideB), "base", "custom"}
		}
	}
	if item.Split {
		for _, option := range []string{string(SideA), string(SideB)} {
			found := false
			for _, existing := range item.Options {
				if existing == option {
					found = true
				}
			}
			if !found && hasBase {
				item.Options = append(item.Options, option)
			}
		}
		sort.Strings(item.Options)
	}
}

// reapplyDecision keeps a finished decision across a rebuild whenever its
// chosen basis still exists with the same value. Decisions whose candidate
// changed are marked stale and must be reviewed; unrelated finished items
// keep their resolution.
func reapplyDecision(item MergeItem, previous MergeItem) MergeItem {
	if previous.Status != MergeResolved && previous.Status != MergeStale {
		return item
	}
	item.Reason = previous.Reason
	switch previous.Choice {
	case string(SideA), string(SideB):
		side := MergeSide(previous.Choice)
		values, ok := item.Values[side]
		if !ok || len(values) == 0 {
			item.Status = MergeStale
			item.Choice = previous.Choice
			item.StaleReason = fmt.Sprintf("chosen side %s no longer carries this object", side)
			return item
		}
		item.Choice = previous.Choice
		oldValues, existed := previous.Values[side]
		// Compare by value set: a rename on the chosen side still invalidates
		// the decision because merged field names follow that side.
		valueChanged := existed && !sameSideValues(oldValues, values)
		renamed := !reflect.DeepEqual(sideFieldNames(item.Fields, side), sideFieldNames(previous.Fields, side))
		if valueChanged || renamed {
			item.Status = MergeStale
			if valueChanged {
				item.StaleReason = fmt.Sprintf("side %s changed after the decision was made", side)
			} else {
				item.StaleReason = fmt.Sprintf("side %s renamed the object after the decision was made", side)
			}
			return item
		}
		item.Status = MergeResolved
	case "base":
		if len(item.Fields[SideBase]) == 0 {
			item.Status = MergeStale
			item.Choice = "base"
			item.StaleReason = "the common base no longer carries this object"
			return item
		}
		item.Choice = "base"
		item.Status = MergeResolved
	case "drop":
		item.Choice = "drop"
		item.Status = MergeResolved
	case "custom":
		item.Choice = "custom"
		item.CustomValue = previous.CustomValue
		item.Status = MergeResolved
	}
	return item
}

// Resolve records the operator's decision for one item. Every manual choice
// must carry a reason so the audit trail explains the reconciliation.
func (s *MergeSession) Resolve(id, choice, reason string, custom any) error {
	if s.adopted {
		return fmt.Errorf("%w: this merge was already adopted; reopen or undo to change it", ErrMergeAdopted)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: every manual resolution must record a reason", ErrInvalidMergeDecision)
	}
	for i := range s.items {
		item := &s.items[i]
		if item.ID != id {
			continue
		}
		switch choice {
		case string(SideA), string(SideB):
			if _, ok := item.Values[MergeSide(choice)]; !ok {
				return fmt.Errorf("%w: item %s has no value on side %s", ErrInvalidMergeDecision, id, choice)
			}
		case "base":
			if len(item.Fields[SideBase]) == 0 {
				return fmt.Errorf("%w: item %s has no common-base value", ErrInvalidMergeDecision, id)
			}
		case "drop":
		case "custom":
		default:
			return fmt.Errorf("%w: unknown choice %q", ErrInvalidMergeDecision, choice)
		}
		item.Choice = choice
		item.CustomValue = custom
		item.Status = MergeResolved
		item.Reason = reason
		item.StaleReason = ""
		s.evaluateRefs()
		return nil
	}
	return fmt.Errorf("%w: merge item %q", ErrMergeItemNotFound, id)
}

// relate links items that carry the same cross-reference, so resolving one
// shows the linked objects whose references must stay consistent.
func (s *MergeSession) relate() {
	related := map[string]map[string]bool{}
	add := func(a, b string) {
		if a == "" || b == "" || a == b {
			return
		}
		if related[a] == nil {
			related[a] = map[string]bool{}
		}
		if related[b] == nil {
			related[b] = map[string]bool{}
		}
		related[a][b] = true
		related[b][a] = true
	}
	for _, ref := range s.refs {
		fromA := s.itemFor(SideA, ref.FromA.File, ref.FromA.Field)
		toA := s.itemFor(SideA, ref.ToA.File, ref.ToA.Field)
		fromB := s.itemFor(SideB, ref.FromB.File, ref.FromB.Field)
		toB := s.itemFor(SideB, ref.ToB.File, ref.ToB.Field)
		ids := uniqueStrings([]string{fromA, toA, fromB, toB})
		for _, x := range ids {
			for _, y := range ids {
				add(x, y)
			}
		}
	}
	for i := range s.items {
		item := &s.items[i]
		item.RelatedIDs = nil
		for id := range related[item.ID] {
			item.RelatedIDs = append(item.RelatedIDs, id)
		}
		sort.Strings(item.RelatedIDs)
	}
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// itemFor maps a side-specific file/field endpoint to a merge item. File
// names select the side ("a", "b", "base", "merged"); the field is the
// name the object carries in that candidate.
func (s *MergeSession) itemFor(implicit MergeSide, file, field string) string {
	side := implicit
	switch file {
	case "a", "b", "base":
		side = MergeSide(file)
	case "merged":
		side = ""
	}
	for _, item := range s.items {
		if side != "" {
			for _, name := range item.Fields[side] {
				if name == field {
					return item.ID
				}
			}
		} else {
			for _, names := range item.Fields {
				for _, name := range names {
					if name == field {
						return item.ID
					}
				}
			}
		}
	}
	return ""
}

func sideValue(doc Document, endpoint FileField) (string, bool) {
	value, ok := doc[endpoint.Field]
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%v", value), true
}

// evaluateRefs checks every declared cross-reference on each candidate and
// against the current draft. A broken reference is a blocker: the merge is
// never adopted into an officially valid-looking but inconsistent result.
func (s *MergeSession) evaluateRefs() {
	merged := s.buildDraft()
	s.refOutcomes = s.refOutcomes[:0]
	for _, ref := range s.refs {
		outcome := MergeRefOutcome{Ref: ref}
		if from, ok := sideValue(s.req.A.Document, ref.FromA); ok {
			outcome.AFrom = from
		}
		if to, ok := sideValue(s.req.A.Document, ref.ToA); ok {
			outcome.ATo = to
		}
		outcome.AOK = outcome.AFrom != "" && outcome.AFrom == outcome.ATo
		if from, ok := sideValue(s.req.B.Document, ref.FromB); ok {
			outcome.BFrom = from
		}
		if to, ok := sideValue(s.req.B.Document, ref.ToB); ok {
			outcome.BTo = to
		}
		outcome.BOK = outcome.BFrom != "" && outcome.BFrom == outcome.BTo

		// Evaluate the draft using each side's endpoint names: an endpoint
		// survives when the chosen side kept that name; otherwise the ref
		// points at a field that no longer exists and is a hard problem.
		check := func(fromEp, toEp FileField) (string, string, bool) {
			fv, fok := lookupMerged(merged, s, fromEp)
			tv, tok := lookupMerged(merged, s, toEp)
			return fv, tv, fok && tok && fv != "" && fv == tv
		}
		fvA, tvA, okA := check(ref.FromA, ref.ToA)
		fvB, tvB, okB := check(ref.FromB, ref.ToB)
		switch {
		case okA:
			outcome.MergedOK = true
			outcome.MergedFrom, outcome.MergedTo = fvA, tvA
		case okB:
			outcome.MergedOK = true
			outcome.MergedFrom, outcome.MergedTo = fvB, tvB
		default:
			outcome.Problem = fmt.Sprintf("reference %q does not resolve in the merged draft", ref.Name)
		}
		ids := uniqueStrings([]string{
			s.itemFor(SideA, ref.FromA.File, ref.FromA.Field),
			s.itemFor(SideA, ref.ToA.File, ref.ToA.Field),
			s.itemFor(SideB, ref.FromB.File, ref.FromB.Field),
			s.itemFor(SideB, ref.ToB.File, ref.ToB.Field),
		})
		outcome.RelatedIDs = ids
		s.refOutcomes = append(s.refOutcomes, outcome)
	}
}

func lookupMerged(merged Document, s *MergeSession, endpoint FileField) (string, bool) {
	if value, ok := merged[endpoint.Field]; ok {
		return fmt.Sprintf("%v", value), true
	}
	// The endpoint field may have been renamed by the chosen resolution:
	// resolve it through the owning merge item.
	side := MergeSide(endpoint.File)
	for _, item := range s.items {
		found := false
		for _, name := range item.Fields[side] {
			if name == endpoint.Field {
				found = true
			}
		}
		if !found {
			continue
		}
		for name := range item.chosenOutputs() {
			if mergedValue, ok := merged[name]; ok {
				return fmt.Sprintf("%v", mergedValue), true
			}
		}
	}
	return "", false
}

func (item MergeItem) chosenOutputs() map[string]any {
	out := map[string]any{}
	switch item.Choice {
	case string(SideA), string(SideB):
		side := MergeSide(item.Choice)
		for name, value := range item.Values[side] {
			out[name] = value
		}
		// Keep the other side's names for fields carrying an identical value,
		// so cross-references expressed in that migration path still resolve
		// instead of dangling after the merge.
		for otherSide, values := range item.Values {
			if otherSide == side {
				continue
			}
			for name, value := range values {
				identical := false
				for _, chosenValue := range out {
					if reflect.DeepEqual(chosenValue, value) {
						identical = true
					}
				}
				if identical {
					out[name] = value
				}
			}
		}
	case "base":
		for _, name := range item.Fields[SideBase] {
			out[name] = item.BaseValue
		}
	case "custom":
		for side, names := range item.Fields {
			if side == SideBase {
				continue
			}
			for _, name := range names {
				out[name] = item.CustomValue
			}
		}
	}
	return out
}

// buildDraft assembles the merged document from current decisions. Open or
// stale items simply contribute nothing yet, so the draft stays partial
// rather than silently guessing.
func (s *MergeSession) buildDraft() Document {
	merged := Document{"version": s.mergedVersion()}
	for _, item := range s.items {
		if item.Status != MergeAuto && item.Status != MergeResolved {
			continue
		}
		for name, value := range item.chosenOutputs() {
			merged[name] = value
		}
	}
	return merged
}

func (s *MergeSession) mergedVersion() string {
	if v, ok := s.req.A.Document["version"].(string); ok && v != "" {
		return v
	}
	if v, ok := s.req.B.Document["version"].(string); ok && v != "" {
		return v
	}
	if v, ok := s.req.Base["version"].(string); ok {
		return v
	}
	return ""
}

// Preview returns the complete reconciliation picture: every object with
// its origins, the paths used on each side, split information, related
// objects and every blocker that keeps the merge unofficial.
func (s *MergeSession) Preview() MergePreview {
	preview := MergePreview{
		Items: append([]MergeItem(nil), s.items...),
		Refs:  append([]MergeRefOutcome(nil), s.refOutcomes...),
		Paths: map[MergeSide][]string{
			SideA: append([]string(nil), s.req.A.Path...),
			SideB: append([]string(nil), s.req.B.Path...),
		},
		Fingerprint: s.fingerprint,
	}
	for _, item := range s.items {
		switch item.Status {
		case MergeUnresolved:
			preview.Blockers = append(preview.Blockers, fmt.Sprintf("item %s (%s) is unresolved", item.ID, item.Kind))
		case MergeStale:
			preview.Blockers = append(preview.Blockers, fmt.Sprintf("item %s must be reviewed again: %s", item.ID, item.StaleReason))
		}
	}
	for _, outcome := range s.refOutcomes {
		if !outcome.MergedOK {
			preview.Blockers = append(preview.Blockers, outcome.Problem)
		}
	}
	sort.Strings(preview.Blockers)
	return preview
}

// Draft returns the partial document produced by current decisions.
func (s *MergeSession) Draft() Document {
	return s.buildDraft()
}

// Item returns one item by ID so callers can inspect one object together
// with everything related to it.
func (s *MergeSession) Item(id string) (MergeItem, error) {
	for _, item := range s.items {
		if item.ID == id {
			return item, nil
		}
	}
	return MergeItem{}, fmt.Errorf("%w: %s", ErrMergeItemNotFound, id)
}

// Related returns the item plus every item linked through cross-references.
func (s *MergeSession) Related(id string) ([]MergeItem, error) {
	target, err := s.Item(id)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{id: true}
	for _, related := range target.RelatedIDs {
		wanted[related] = true
	}
	var out []MergeItem
	for _, item := range s.items {
		if wanted[item.ID] {
			out = append(out, item)
		}
	}
	return out, nil
}

// UpdateCandidates replaces one or both candidate documents because a team
// kept editing during reconciliation. Only decisions based on changed
// values/names are marked stale; finished unrelated choices are preserved.
func (s *MergeSession) UpdateCandidates(a, b Document) (MergePreview, error) {
	if s.adopted {
		return MergePreview{}, fmt.Errorf("%w: reopen the merge before changing candidates", ErrMergeAdopted)
	}
	req := s.req
	if a != nil {
		req.A.Document = a
	}
	if b != nil {
		req.B.Document = b
	}
	// Validate the new candidates still match their documented paths.
	if a != nil {
		docA, linA, err := s.engine.lineageFor(context.Background(), req.Base, req.A.From, req.A.Path, SideA)
		if err != nil {
			return MergePreview{}, fmt.Errorf("candidate a: %w", err)
		}
		if err := validateCandidate(stripVersion(docA), stripVersion(a), linA); err != nil {
			return MergePreview{}, fmt.Errorf("%w: %v", ErrStalePreview, err)
		}
	}
	if b != nil {
		docB, linB, err := s.engine.lineageFor(context.Background(), req.Base, req.B.From, req.B.Path, SideB)
		if err != nil {
			return MergePreview{}, fmt.Errorf("candidate b: %w", err)
		}
		if err := validateCandidate(stripVersion(docB), stripVersion(b), linB); err != nil {
			return MergePreview{}, fmt.Errorf("%w: %v", ErrStalePreview, err)
		}
	}
	s.req = req
	s.fingerprint = mergeFingerprint(req, s.refs)
	s.rebuild()
	return s.Preview(), nil
}

// MergeResult is the official, fully verified merge.
type MergeResult struct {
	Document Document
	Preview  MergePreview
}

// Commit adopts the draft only when every item is resolved and every cross-
// reference still resolves. Nothing is published on any blocker, so a
// superficially valid document with broken references can never become the
// official version.
func (s *MergeSession) Commit(ctx context.Context, publish func(context.Context, MergeRelease) error) (MergeResult, error) {
	if err := ctx.Err(); err != nil {
		return MergeResult{}, err
	}
	preview := s.Preview()
	if len(preview.Blockers) > 0 {
		return MergeResult{}, fmt.Errorf("%w: %s", ErrMergeBlocked, strings.Join(preview.Blockers, "; "))
	}
	document := s.buildDraft()
	result := MergeResult{Document: document, Preview: preview}
	if publish != nil {
		release := s.release(document, preview)
		if err := publish(ctx, release); err != nil {
			return MergeResult{}, fmt.Errorf("publish merge: %w", err)
		}
		if s.onAdopt != nil {
			if err := s.onAdopt(release); err != nil {
				return MergeResult{}, fmt.Errorf("record merge adoption: %w", err)
			}
		}
	}
	s.adopted = true
	return result, nil
}

// Reopen returns an adopted session to the unresolved state for another
// round. The original candidates, every recorded decision, and its reasons
// are all retained by the store; reopening only flips the working flag.
func (s *MergeSession) Reopen() {
	s.adopted = false
}

// Adopted reports whether this session's merge has been officially taken.
func (s *MergeSession) Adopted() bool { return s.adopted }

// Request returns the two candidates and their common base.
func (s *MergeSession) Request() MergeRequest { return s.req }

// Refs returns the declared cross-references.
func (s *MergeSession) Refs() []MergeRef { return append([]MergeRef(nil), s.refs...) }

// Fingerprint binds stored decisions to the exact current candidates.
func (s *MergeSession) Fingerprint() string { return s.fingerprint }

// MergeRelease is handed to the publisher after a fully verified merge.
type MergeRelease struct {
	Base     Document     `json:"-"`
	A        Document     `json:"-"`
	B        Document     `json:"-"`
	Document Document     `json:"-"`
	Preview  MergePreview `json:"preview"`
}

func (s *MergeSession) release(document Document, preview MergePreview) MergeRelease {
	return MergeRelease{
		Base:     s.req.Base.Clone(),
		A:        s.req.A.Document.Clone(),
		B:        s.req.B.Document.Clone(),
		Document: document.Clone(),
		Preview:  preview,
	}
}
