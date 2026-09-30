package configmigrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// Document is a complete versioned application configuration.
type Document map[string]any

// Clone returns an independent copy of the document. Maps and slices are copied recursively.
func (d Document) Clone() Document {
	if d == nil {
		return nil
	}
	return cloneValue(d).(Document)
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case Document:
		result := make(Document, len(typed))
		for key, item := range typed {
			result[key] = cloneValue(item)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = cloneValue(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for i := range typed {
			result[i] = cloneValue(typed[i])
		}
		return result
	default:
		return value
	}
}

// Step migrates a complete document from one version to the next version.
type Step struct {
	From        string
	To          string
	Description string
	// Migrate receives a private working copy and must not mutate its input in place.
	Migrate func(context.Context, Document) (Document, error)
	// MigrateDoc is like Migrate and additionally reports field-level changes.
	MigrateDoc func(context.Context, Document) (StepResult, error)
	// Reverse is optional. When absent, this edge cannot be used for downgrades.
	Reverse func(context.Context, Document) (Document, error)
	// ReverseDoc is like Reverse and additionally reports field-level changes.
	ReverseDoc func(context.Context, Document) (StepResult, error)
}

func (s Step) validate() error {
	if s.From == "" || s.To == "" {
		return fmt.Errorf("%w: source and target versions are required", ErrInvalidStep)
	}
	if s.From == s.To {
		return fmt.Errorf("%w: source and target version %q are equal", ErrInvalidStep, s.From)
	}
	if s.Migrate == nil && s.MigrateDoc == nil {
		return fmt.Errorf("%w: migration function is required for %s -> %s", ErrInvalidStep, s.From, s.To)
	}
	return nil
}

const (
	SourceOldValue = "old_value"
	SourceDefault  = "default"
	SourceSplit    = "split_rule"
	SourceComputed = "computed"
	SourceUnknown  = "unknown"
)

const (
	ActionAdd    = "add"
	ActionChange = "change"
	ActionRemove = "remove"
)

// FieldSource identifies one old value, default, or rule that produced a field.
type FieldSource struct {
	Kind  string `json:"kind"`
	Field string `json:"field,omitempty"`
	Value any    `json:"value,omitempty"`
	Rule  string `json:"rule,omitempty"`
}

// FieldChange is an auditable explanation of one field-level migration effect.
type FieldChange struct {
	Field       string        `json:"field"`
	Action      string        `json:"action"`
	Before      any           `json:"before"`
	After       any           `json:"after"`
	Sources     []FieldSource `json:"sources,omitempty"`
	Loss        bool          `json:"loss,omitempty"`
	Description string        `json:"description,omitempty"`
}

// StepResult contains a migrated document and its field-level report.
type StepResult struct {
	Document Document
	Changes  []FieldChange
}

// StepChange groups field changes under the migration record that produced them.
type StepChange struct {
	Step    Record        `json:"step"`
	Changes []FieldChange `json:"changes"`
}

// Record is an auditable entry for one successful migration.
type Record struct {
	Timestamp   time.Time `json:"timestamp"`
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	Description string    `json:"description,omitempty"`
}

// Release is handed to a publisher only after every migration step has succeeded.
type Release struct {
	Original Document `json:"-"`
	Document Document `json:"-"`
	Record   Record   `json:"record"`
	Path     []Record `json:"path"`
	Result   Result   `json:"result"`
}

// Result describes an in-memory migration.
type Result struct {
	FromVersion string
	ToVersion   string
	Document    Document
	Records     []Record
	Changes     []FieldChange
	Steps       []StepChange
	Lossy       bool
	Migrated    bool
}

// Preview is a verified impact report that can later be committed.
type Preview struct {
	FromVersion       string    `json:"from_version"`
	ToVersion         string    `json:"to_version"`
	Path              []string  `json:"path"`
	Result            Result    `json:"result"`
	InputFingerprint  string    `json:"input_fingerprint"`
	OutputFingerprint string    `json:"output_fingerprint"`
	CreatedAt         time.Time `json:"created_at"`
}

type migrateOptions struct {
	explicitPath []string
	now          func() time.Time
}

// Option customizes migration planning and execution.
type Option func(*migrateOptions)

// WithPath explicitly selects a branch. The first and last entries must be the current and target versions.
func WithPath(versions ...string) Option {
	return func(options *migrateOptions) {
		options.explicitPath = append([]string(nil), versions...)
	}
}

func withClock(now func() time.Time) Option {
	return func(options *migrateOptions) {
		options.now = now
	}
}

// Engine executes registered version migration steps.
type Engine struct {
	steps map[string]map[string]Step
}

// NewEngine creates an empty migration engine.
func NewEngine() *Engine {
	return &Engine{steps: make(map[string]map[string]Step)}
}

// Register adds a migration step. A later registration for the same version pair replaces the earlier step.
func (e *Engine) Register(step Step) error {
	if err := step.validate(); err != nil {
		return err
	}
	if e.steps[step.From] == nil {
		e.steps[step.From] = make(map[string]Step)
	}
	e.steps[step.From][step.To] = step
	return nil
}

// MustRegister registers a step or panics on invalid registration.
func (e *Engine) MustRegister(step Step) {
	if err := e.Register(step); err != nil {
		panic(err)
	}
}

// Plan resolves and validates the ordered steps before any document is changed or published.
func (e *Engine) Plan(from, to string, options ...Option) ([]Step, error) {
	opts := migrateOptions{now: time.Now}
	for _, option := range options {
		option(&opts)
	}
	if from == "" || to == "" {
		return nil, fmt.Errorf("%w: source and target versions are required", ErrInvalidStep)
	}
	if from == to {
		return nil, fmt.Errorf("%w: %q", ErrAlreadyAtTarget, from)
	}

	versions, err := e.resolvePath(from, to, opts.explicitPath)
	if err != nil {
		return nil, err
	}
	steps := make([]Step, 0, len(versions)-1)
	for i := 0; i < len(versions)-1; i++ {
		steps = append(steps, e.steps[versions[i]][versions[i+1]])
	}
	return steps, nil
}

func (e *Engine) resolvePath(from, to string, explicit []string) ([]string, error) {
	if len(explicit) > 0 {
		if explicit[0] != from || explicit[len(explicit)-1] != to {
			return nil, fmt.Errorf("%w: explicit path must start at %q and end at %q", ErrInvalidStep, from, to)
		}
		for i := 0; i < len(explicit)-1; i++ {
			if e.steps[explicit[i]] == nil || !e.steps[explicit[i]][explicit[i+1]].supportsForward() {
				return nil, fmt.Errorf("%w: missing step %s -> %s", ErrNoMigrationPath, explicit[i], explicit[i+1])
			}
		}
		return explicit, nil
	}

	paths := e.allPaths(from, to, 256)
	switch len(paths) {
	case 0:
		return nil, fmt.Errorf("%w: %s -> %s", ErrNoMigrationPath, from, to)
	case 1:
		return paths[0], nil
	default:
		return nil, fmt.Errorf("%w: %s -> %q has %d candidates; use WithPath to choose one: %v", ErrAmbiguousPath, from, to, len(paths), paths)
	}
}

func (s Step) supportsForward() bool {
	return s.Migrate != nil || s.MigrateDoc != nil
}

func (s Step) supportsReverse() bool {
	return s.Reverse != nil || s.ReverseDoc != nil
}

func (e *Engine) allPaths(from, to string, maxPaths int) [][]string {
	var result [][]string
	visited := map[string]bool{from: true}
	current := []string{from}
	var walk func(string)
	walk = func(version string) {
		if len(result) >= maxPaths {
			return
		}
		if version == to {
			result = append(result, append([]string(nil), current...))
			return
		}
		for next := range e.steps[version] {
			if visited[next] {
				continue
			}
			visited[next] = true
			current = append(current, next)
			walk(next)
			current = current[:len(current)-1]
			visited[next] = false
		}
	}

	walk(from)
	return result
}

// Preview resolves and runs a migration without publishing it.
func (e *Engine) Preview(ctx context.Context, doc Document, target string, options ...Option) (Preview, error) {
	if doc == nil {
		return Preview{}, fmt.Errorf("%w: document is required", ErrInvalidStep)
	}
	from, ok := doc["version"].(string)
	if !ok || from == "" {
		return Preview{}, fmt.Errorf("%w: document is missing a string version field", ErrInvalidStep)
	}
	opts := migrateOptions{now: time.Now}
	for _, option := range options {
		option(&opts)
	}
	steps, err := e.Plan(from, target, options...)
	if err != nil {
		return Preview{}, err
	}
	result, err := e.executeSteps(ctx, doc, from, target, steps, opts.now, false)
	if err != nil {
		return Preview{}, err
	}
	inputFingerprint, err := fingerprint(doc)
	if err != nil {
		return Preview{}, err
	}
	outputFingerprint, err := fingerprint(result.Document)
	if err != nil {
		return Preview{}, err
	}
	return Preview{
		FromVersion:       from,
		ToVersion:         target,
		Path:              pathFromSteps(steps),
		Result:            result,
		InputFingerprint:  inputFingerprint,
		OutputFingerprint: outputFingerprint,
		CreatedAt:         opts.now().UTC(),
	}, nil
}

// ApplyPreview runs the previewed migration only when the current input is unchanged.
func (e *Engine) ApplyPreview(ctx context.Context, doc Document, preview Preview, options ...Option) (Result, error) {
	return e.applyPreview(ctx, doc, preview, nil, options...)
}

// PublishPreview runs the previewed migration and publishes it only when the input is unchanged.
func (e *Engine) PublishPreview(ctx context.Context, doc Document, preview Preview, publish func(context.Context, Release) error, options ...Option) (Result, error) {
	return e.applyPreview(ctx, doc, preview, publish, options...)
}

func (e *Engine) applyPreview(ctx context.Context, doc Document, preview Preview, publish func(context.Context, Release) error, options ...Option) (Result, error) {
	if doc == nil {
		return Result{}, fmt.Errorf("%w: document is required", ErrInvalidStep)
	}
	from, ok := doc["version"].(string)
	if !ok || from == "" {
		return Result{}, fmt.Errorf("%w: document is missing a string version field", ErrInvalidStep)
	}
	if from != preview.FromVersion || preview.ToVersion == "" {
		return Result{}, fmt.Errorf("%w: current version or target differs", ErrPreviewMismatch)
	}
	previewOptions := append([]Option{WithPath(preview.Path...)}, options...)
	steps, err := e.Plan(from, preview.ToVersion, previewOptions...)
	if err != nil {
		return Result{}, err
	}
	if !equalStrings(pathFromSteps(steps), preview.Path) {
		return Result{}, fmt.Errorf("%w: resolved path differs", ErrPreviewMismatch)
	}
	inputFingerprint, err := fingerprint(doc)
	if err != nil {
		return Result{}, err
	}
	if inputFingerprint != preview.InputFingerprint {
		return Result{}, fmt.Errorf("%w: input changed after preview", ErrPreviewStale)
	}
	opts := migrateOptions{now: time.Now}
	for _, option := range options {
		option(&opts)
	}
	result, err := e.executeSteps(ctx, doc, from, preview.ToVersion, steps, opts.now, false)
	if err != nil {
		return Result{}, err
	}
	outputFingerprint, err := fingerprint(result.Document)
	if err != nil {
		return Result{}, err
	}
	if outputFingerprint != preview.OutputFingerprint {
		return Result{}, fmt.Errorf("%w: output changed after preview", ErrPreviewStale)
	}
	if publish != nil {
		if err := e.commit(ctx, doc, result, publish); err != nil {
			return result, err
		}
	}
	return result, nil
}

func pathFromSteps(steps []Step) []string {
	if len(steps) == 0 {
		return nil
	}
	path := make([]string, 0, len(steps)+1)
	path = append(path, steps[0].From)
	for _, step := range steps {
		path = append(path, step.To)
	}
	return path
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func fingerprint(doc Document) (string, error) {
	contents, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, contents); err != nil {
		return "", err
	}
	hash := sha256.Sum256(compact.Bytes())
	return hex.EncodeToString(hash[:]), nil
}

// Migrate runs a complete migration on a private copy. The input document is never mutated.
func (e *Engine) Migrate(ctx context.Context, doc Document, target string, options ...Option) (Result, error) {
	if doc == nil {
		return Result{}, fmt.Errorf("%w: document is required", ErrInvalidStep)
	}
	from, ok := doc["version"].(string)
	if !ok || from == "" {
		return Result{}, fmt.Errorf("%w: document is missing a string version field", ErrInvalidStep)
	}
	if from == target {
		return Result{
			FromVersion: from,
			ToVersion:   target,
			Document:    doc.Clone(),
			Migrated:    false,
		}, nil
	}

	steps, err := e.Plan(from, target, options...)
	if err != nil {
		return Result{}, err
	}
	opts := migrateOptions{now: time.Now}
	for _, option := range options {
		option(&opts)
	}

	return e.executeSteps(ctx, doc, from, target, steps, opts.now, false)
}

func (e *Engine) executeSteps(ctx context.Context, original Document, from, target string, steps []Step, now func() time.Time, reverse bool) (result Result, err error) {
	working := original.Clone()
	records := make([]Record, 0, len(steps))
	stepChanges := make([]StepChange, 0, len(steps))
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		before := working.Clone()
		stepResult, err := runMigrationStep(ctx, step, working, reverse)
		if err != nil {
			direction := "migration"
			if reverse {
				direction = "reverse migration"
			}
			return Result{}, fmt.Errorf("%s %s -> %s failed: %w", direction, step.From, step.To, err)
		}
		if stepResult.Document == nil {
			return Result{}, fmt.Errorf("migration %s -> %s failed: returned a nil document", step.From, step.To)
		}
		if version, _ := stepResult.Document["version"].(string); version != step.To {
			return Result{}, fmt.Errorf("migration %s -> %s failed: resulting version is %q", step.From, step.To, version)
		}
		changes := normalizeChanges(before, stepResult.Document, stepResult.Changes)
		working = stepResult.Document
		record := Record{
			Timestamp:   now().UTC(),
			FromVersion: step.From,
			ToVersion:   step.To,
			Description: step.Description,
		}
		records = append(records, record)
		if len(changes) > 0 {
			stepChanges = append(stepChanges, StepChange{Step: record, Changes: changes})
		}
	}

	changes, lossy := aggregateChanges(stepChanges)
	return Result{
		FromVersion: from,
		ToVersion:   target,
		Document:    working,
		Records:     records,
		Changes:     changes,
		Steps:       stepChanges,
		Lossy:       lossy,
		Migrated:    true,
	}, nil
}

func runMigrationStep(ctx context.Context, step Step, doc Document, reverse bool) (result StepResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("migration %s -> %s panicked: %v", step.From, step.To, recovered)
			result = StepResult{}
		}
	}()
	if reverse {
		if step.ReverseDoc != nil {
			return step.ReverseDoc(ctx, doc)
		}
		document, err := step.Reverse(ctx, doc)
		return StepResult{Document: document}, err
	}
	if step.MigrateDoc != nil {
		return step.MigrateDoc(ctx, doc)
	}
	document, err := step.Migrate(ctx, doc)
	return StepResult{Document: document}, err
}

func normalizeChanges(before, after Document, reported []FieldChange) []FieldChange {
	index := map[string]FieldChange{}
	order := make([]string, 0, len(reported))
	for _, change := range reported {
		if change.Field == "" {
			continue
		}
		if _, exists := index[change.Field]; !exists {
			order = append(order, change.Field)
		}
		current := index[change.Field]
		current.Field = change.Field
		if change.Action != "" {
			current.Action = change.Action
		}
		if change.Before != nil || existsValue(before, change.Field) {
			current.Before = change.Before
		}
		if change.After != nil || existsValue(after, change.Field) {
			current.After = change.After
		}
		current.Sources = append(append([]FieldSource(nil), current.Sources...), change.Sources...)
		current.Loss = current.Loss || change.Loss
		if change.Description != "" {
			current.Description = change.Description
		}
		index[change.Field] = current
	}

	keys := append(fieldKeys(before), fieldKeys(after)...)
	sortStrings(keys)
	keys = uniqueSorted(keys)
	result := make([]FieldChange, 0, len(keys))
	for _, key := range keys {
		beforeValue, beforeOK := before[key]
		afterValue, afterOK := after[key]
		if beforeOK && afterOK && valuesEqual(beforeValue, afterValue) {
			continue
		}
		change, exists := index[key]
		if !exists {
			change = FieldChange{Field: key}
			order = append(order, key)
		}
		switch {
		case !beforeOK && afterOK:
			if change.Action == "" {
				change.Action = ActionAdd
			}
			change.After = afterValue
		case beforeOK && !afterOK:
			if change.Action == "" {
				change.Action = ActionRemove
			}
			change.Before = beforeValue
			change.Loss = change.Loss || !hasReportedField(reported, key)
		default:
			if change.Action == "" {
				change.Action = ActionChange
			}
			change.Before = beforeValue
			change.After = afterValue
		}
		if len(change.Sources) == 0 {
			sourceKind := SourceUnknown
			if change.Action == ActionChange {
				sourceKind = SourceOldValue
			}
			change.Sources = []FieldSource{{Kind: sourceKind}}
		}
		result = append(result, change)
	}
	return result
}

func aggregateChanges(steps []StepChange) ([]FieldChange, bool) {
	index := map[string]FieldChange{}
	existed := map[string]bool{}
	currentlyPresent := map[string]bool{}
	order := []string{}
	lossy := false
	for _, step := range steps {
		for _, change := range step.Changes {
			current, exists := index[change.Field]
			if !exists {
				order = append(order, change.Field)
				current = FieldChange{Field: change.Field}
			}
			if change.Action == ActionRemove {
				if !existed[change.Field] {
					current.Before = change.Before
				}
				current.Action = ActionRemove
				current.After = nil
				existed[change.Field] = true
				currentlyPresent[change.Field] = false
			} else {
				if !existed[change.Field] && change.Action != ActionAdd {
					current.Before = change.Before
				}
				if existed[change.Field] || change.Action == ActionChange {
					current.Action = ActionChange
				} else {
					current.Action = ActionAdd
				}
				current.After = change.After
				existed[change.Field] = true
				currentlyPresent[change.Field] = true
			}
			current.Sources = append(append([]FieldSource(nil), current.Sources...), change.Sources...)
			current.Loss = current.Loss || change.Loss
			lossy = lossy || current.Loss
			if change.Description != "" {
				current.Description = change.Description
			}
			index[change.Field] = current
		}
	}
	result := make([]FieldChange, 0, len(order))
	for _, key := range order {
		result = append(result, index[key])
	}
	return result, lossy
}

func hasReportedField(changes []FieldChange, field string) bool {
	for _, change := range changes {
		if change.Field == field {
			return true
		}
	}
	return false
}

func existsValue(doc Document, field string) bool {
	_, ok := doc[field]
	return ok
}

func fieldKeys(doc Document) []string {
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	return keys
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func valuesEqual(left, right any) bool {
	return reflect.DeepEqual(left, right)
}

// Publish migrates in memory and invokes publish only when the entire migration succeeded.
func (e *Engine) Publish(ctx context.Context, doc Document, target string, publish func(context.Context, Release) error, options ...Option) (Result, error) {
	result, err := e.Migrate(ctx, doc, target, options...)
	if err != nil || !result.Migrated || publish == nil {
		return result, err
	}
	if err := e.commit(ctx, doc, result, publish); err != nil {
		return result, err
	}
	return result, nil
}

func (e *Engine) commit(ctx context.Context, doc Document, result Result, publish func(context.Context, Release) error) error {
	release := Release{
		Original: doc.Clone(),
		Document: result.Document,
		Path:     result.Records,
		Result:   result,
	}
	if len(result.Records) > 0 {
		release.Record = result.Records[len(result.Records)-1]
	}
	if err := publish(ctx, release); err != nil {
		return fmt.Errorf("publish migration %s -> %s: %w", result.FromVersion, result.ToVersion, err)
	}
	return nil
}

// Reverse migrates along registered Reverse implementations.
func (e *Engine) Reverse(ctx context.Context, doc Document, target string, options ...Option) (Result, error) {
	from, ok := doc["version"].(string)
	if !ok || from == "" {
		return Result{}, fmt.Errorf("%w: document is missing a string version field", ErrInvalidStep)
	}
	if from == target {
		return Result{
			FromVersion: from,
			ToVersion:   target,
			Document:    doc.Clone(),
			Migrated:    false,
		}, nil
	}

	opts := migrateOptions{now: time.Now}
	for _, option := range options {
		option(&opts)
	}
	versions, err := e.resolveReversePath(from, target, opts.explicitPath)
	if err != nil {
		return Result{}, err
	}
	steps := make([]Step, 0, len(versions)-1)
	for i := 0; i < len(versions)-1; i++ {
		step := e.steps[versions[i+1]][versions[i]]
		reverseStep := Step{
			From:        step.To,
			To:          step.From,
			Description: "reverse: " + step.Description,
			ReverseDoc:  step.ReverseDoc,
			Reverse:     step.Reverse,
			MigrateDoc:  step.ReverseDoc,
			Migrate:     nil,
		}
		if step.Reverse != nil {
			reverseStep.Migrate = func(ctx context.Context, doc Document) (Document, error) {
				return step.Reverse(ctx, doc)
			}
		}
		steps = append(steps, reverseStep)
	}
	return e.executeSteps(ctx, doc, from, target, steps, opts.now, true)
}

// ReverseBlocker explains why a downgrade path is unavailable.
type ReverseBlocker struct {
	Path   []string `json:"path,omitempty"`
	From   string   `json:"from,omitempty"`
	To     string   `json:"to,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// ReverseBlockers is the structured cause wrapped with ErrReverseUnsupported.
type ReverseBlockers []ReverseBlocker

func (blockers ReverseBlockers) Error() string {
	if len(blockers) == 0 {
		return "no reverse path blockers were reported"
	}
	return fmt.Sprintf("reverse migration %s -> %s is blocked at %s -> %s: %s",
		blockers[0].Path[0], blockers[0].Path[len(blockers[0].Path)-1],
		blockers[0].From, blockers[0].To, blockers[0].Reason)
}

// PreviewReverse validates a rollback and returns its impact without executing a publisher.
func (e *Engine) PreviewReverse(ctx context.Context, doc Document, target string, options ...Option) (Preview, error) {
	result, err := e.Reverse(ctx, doc, target, options...)
	if err != nil {
		return Preview{}, err
	}
	inputFingerprint, err := fingerprint(doc)
	if err != nil {
		return Preview{}, err
	}
	outputFingerprint, err := fingerprint(result.Document)
	if err != nil {
		return Preview{}, err
	}
	opts := migrateOptions{now: time.Now}
	for _, option := range options {
		option(&opts)
	}
	path := make([]string, 0, len(result.Records)+1)
	if len(result.Records) > 0 {
		path = append(path, result.Records[0].FromVersion)
		for _, record := range result.Records {
			path = append(path, record.ToVersion)
		}
	}
	return Preview{
		FromVersion:       result.FromVersion,
		ToVersion:         result.ToVersion,
		Path:              path,
		Result:            result,
		InputFingerprint:  inputFingerprint,
		OutputFingerprint: outputFingerprint,
		CreatedAt:         opts.now().UTC(),
	}, nil
}

// ApplyReversePreview runs the previewed rollback only when the current input is unchanged.
func (e *Engine) ApplyReversePreview(ctx context.Context, doc Document, preview Preview, options ...Option) (Result, error) {
	return e.applyReversePreview(ctx, doc, preview, nil, options...)
}

// PublishReversePreview runs and publishes the previewed rollback only when the input is unchanged.
func (e *Engine) PublishReversePreview(ctx context.Context, doc Document, preview Preview, publish func(context.Context, Release) error, options ...Option) (Result, error) {
	return e.applyReversePreview(ctx, doc, preview, publish, options...)
}

func (e *Engine) applyReversePreview(ctx context.Context, doc Document, preview Preview, publish func(context.Context, Release) error, options ...Option) (Result, error) {
	if doc == nil {
		return Result{}, fmt.Errorf("%w: document is required", ErrInvalidStep)
	}
	from, ok := doc["version"].(string)
	if !ok || from == "" {
		return Result{}, fmt.Errorf("%w: document is missing a string version field", ErrInvalidStep)
	}
	if from != preview.FromVersion || preview.ToVersion == "" {
		return Result{}, fmt.Errorf("%w: current version or target differs", ErrPreviewMismatch)
	}
	inputFingerprint, err := fingerprint(doc)
	if err != nil {
		return Result{}, err
	}
	if inputFingerprint != preview.InputFingerprint {
		return Result{}, fmt.Errorf("%w: input changed after preview", ErrPreviewStale)
	}
	applyOptions := append([]Option{WithPath(preview.Path...)}, options...)
	result, err := e.Reverse(ctx, doc, preview.ToVersion, applyOptions...)
	if err != nil {
		return Result{}, err
	}
	outputFingerprint, err := fingerprint(result.Document)
	if err != nil {
		return Result{}, err
	}
	if outputFingerprint != preview.OutputFingerprint {
		return Result{}, fmt.Errorf("%w: output changed after preview", ErrPreviewStale)
	}
	if publish != nil {
		if err := e.commit(ctx, doc, result, publish); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (e *Engine) resolveReversePath(from, to string, explicit []string) ([]string, error) {
	validateExplicit := func(versions []string) error {
		if len(versions) < 2 || versions[0] != from || versions[len(versions)-1] != to {
			return fmt.Errorf("%w: explicit reverse path must start at %q and end at %q", ErrInvalidStep, from, to)
		}
		for i := 0; i < len(versions)-1; i++ {
			if forward := e.steps[versions[i+1]][versions[i]]; !forward.supportsReverse() {
				return fmt.Errorf("%w for %s -> %s: %w", ErrReverseUnsupported, from, to,
					ReverseBlockers{{Path: append([]string(nil), versions...), From: versions[i], To: versions[i+1], Reason: "forward step has no reverse implementation"}})
			}
		}
		return nil
	}
	if len(explicit) > 0 {
		if err := validateExplicit(explicit); err != nil {
			return nil, err
		}
		return append([]string(nil), explicit...), nil
	}

	paths := e.allReversePaths(from, to, 256)
	supported := make([][]string, 0, len(paths))
	for _, candidate := range paths {
		if err := validateExplicit(candidate); err == nil {
			supported = append(supported, candidate)
		}
	}
	if len(supported) == 1 {
		return supported[0], nil
	}
	if len(supported) > 1 {
		return nil, fmt.Errorf("%w for %s -> %s: %w", ErrReverseUnsupported, from, to, ReverseBlockers{{From: from, To: to, Reason: fmt.Sprintf("%d reverse paths are available; use WithPath to choose one: %v", len(supported), supported)}})
	}
	blockers := e.reverseBlockers(from, to, paths)
	if len(blockers) == 0 {
		blockers = append(blockers, ReverseBlocker{From: from, To: to, Reason: "no reachable reverse path"})
	}
	return nil, fmt.Errorf("%w for %s -> %s: %w", ErrReverseUnsupported, from, to, blockers)
}

func (e *Engine) allReversePaths(from, to string, maxPaths int) [][]string {
	result := [][]string{}
	visited := map[string]bool{from: true}
	current := []string{from}
	var walk func(string)
	walk = func(version string) {
		if len(result) >= maxPaths {
			return
		}
		if version == to {
			result = append(result, append([]string(nil), current...))
			return
		}
		for predecessor, step := range e.steps {
			if _, outgoing := step[version]; !outgoing {
				continue
			}
			if visited[predecessor] {
				continue
			}
			visited[predecessor] = true
			current = append(current, predecessor)
			walk(predecessor)
			current = current[:len(current)-1]
			visited[predecessor] = false
		}
	}
	walk(from)
	return result
}

func (e *Engine) reverseBlockers(from, to string, paths [][]string) ReverseBlockers {
	blockers := ReverseBlockers{}
	for _, candidate := range paths {
		for i := 0; i < len(candidate)-1; i++ {
			if forward := e.steps[candidate[i+1]][candidate[i]]; !forward.supportsReverse() {
				blockers = append(blockers, ReverseBlocker{
					Path:   append([]string(nil), candidate...),
					From:   candidate[i],
					To:     candidate[i+1],
					Reason: "forward step has no reverse implementation",
				})
				break
			}
		}
	}
	return blockers
}
