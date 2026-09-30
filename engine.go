package configmigrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// Reverse is optional. When absent, this edge cannot be used for downgrades.
	Reverse func(context.Context, Document) (Document, error)
	// Effects optionally declares how the step produces or removes fields so
	// previews can explain field provenance (default, split rule, ...).
	Effects []FieldEffect
}

func (s Step) validate() error {
	if s.From == "" || s.To == "" {
		return fmt.Errorf("%w: source and target versions are required", ErrInvalidStep)
	}
	if s.From == s.To {
		return fmt.Errorf("%w: source and target version %q are equal", ErrInvalidStep, s.From)
	}
	if s.Migrate == nil {
		return fmt.Errorf("%w: migration function is required for %s -> %s", ErrInvalidStep, s.From, s.To)
	}
	return nil
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
}

// Result describes an in-memory migration.
type Result struct {
	FromVersion string
	ToVersion   string
	Document    Document
	Records     []Record
	Migrated    bool
	// Warnings lists information loss observed during migration, such as
	// fields that were dropped or replaced and cannot be reconstructed.
	Warnings []string
}

var timeNow = time.Now

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
	opts := migrateOptions{now: timeNow}
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
			if e.steps[explicit[i]] == nil || e.steps[explicit[i]][explicit[i+1]].Migrate == nil {
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
	opts := migrateOptions{now: timeNow}
	for _, option := range options {
		option(&opts)
	}

	working := doc.Clone()
	records := make([]Record, 0, len(steps))
	var warnings []string
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		next, err := runStep(ctx, step, working)
		if err != nil {
			return Result{}, fmt.Errorf("migration %s -> %s failed: %w", step.From, step.To, err)
		}
		if next == nil {
			return Result{}, fmt.Errorf("migration %s -> %s failed: returned a nil document", step.From, step.To)
		}
		if version, _ := next["version"].(string); version != step.To {
			return Result{}, fmt.Errorf("migration %s -> %s failed: resulting version is %q", step.From, step.To, version)
		}
		_, losses := stepProvenance(step, working, next)
		warnings = append(warnings, losses...)
		working = next
		records = append(records, Record{
			Timestamp:   opts.now().UTC(),
			FromVersion: step.From,
			ToVersion:   step.To,
			Description: step.Description,
		})
	}

	return Result{
		FromVersion: from,
		ToVersion:   target,
		Document:    working,
		Records:     records,
		Migrated:    true,
		Warnings:    warnings,
	}, nil
}

func runStep(ctx context.Context, step Step, doc Document) (result Document, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("migration %s -> %s panicked: %v", step.From, step.To, recovered)
			result = nil
		}
	}()
	return step.Migrate(ctx, doc)
}

// Publish migrates in memory and invokes publish only when the entire migration succeeded.
func (e *Engine) Publish(ctx context.Context, doc Document, target string, publish func(context.Context, Release) error, options ...Option) (Result, error) {
	result, err := e.Migrate(ctx, doc, target, options...)
	if err != nil || !result.Migrated {
		return result, err
	}
	release := Release{
		Original: doc.Clone(),
		Document: result.Document,
		Path:     result.Records,
	}
	if len(result.Records) > 0 {
		release.Record = result.Records[len(result.Records)-1]
	}
	if err := publish(ctx, release); err != nil {
		return Result{}, fmt.Errorf("publish migration %s -> %s: %w", result.FromVersion, result.ToVersion, err)
	}
	return result, nil
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

	reverseEngine := NewEngine()
	for _, outgoing := range e.steps {
		for _, step := range outgoing {
			if step.Reverse == nil {
				continue
			}
			reverseStep := Step{
				From:        step.To,
				To:          step.From,
				Description: "reverse: " + step.Description,
				Migrate:     step.Reverse,
			}
			reverseEngine.MustRegister(reverseStep)
		}
	}
	result, err := reverseEngine.Migrate(ctx, doc, target, options...)
	if errors.Is(err, ErrNoMigrationPath) || errors.Is(err, ErrAmbiguousPath) {
		blockers := e.RollbackBlockers(from, target)
		if len(blockers) == 0 {
			blockers = []string{"no registered reverse path connects the two versions"}
		}
		return Result{}, fmt.Errorf("%w for %s -> %s: %s", ErrReverseUnsupported, from, target, strings.Join(blockers, "; "))
	}
	if err != nil {
		return Result{}, err
	}
	// A downgrade is not guaranteed to be lossless: report every field that
	// existed before the rollback and is gone afterwards instead of pretending
	// the original configuration was fully restored.
	for key := range doc {
		if key == "version" {
			continue
		}
		if _, kept := result.Document[key]; !kept {
			result.Warnings = append(result.Warnings, fmt.Sprintf("field %q was dropped by the rollback and is not restored", key))
		}
	}
	return result, nil
}

// RollbackBlockers reports the concrete reasons a downgrade from -> to cannot
// run: each forward step on the path that does not implement Reverse, or the
// absence of a forward path at all. It returns nil when a rollback is possible.
func (e *Engine) RollbackBlockers(from, to string) []string {
	paths := e.allPaths(to, from, 256)
	if len(paths) == 0 {
		return []string{fmt.Sprintf("no migration path %s -> %s exists in either direction", from, to)}
	}
	var best []string
	for _, path := range paths {
		var blockers []string
		for i := 0; i < len(path)-1; i++ {
			step := e.steps[path[i]][path[i+1]]
			if step.Reverse == nil {
				blockers = append(blockers, fmt.Sprintf("step %s -> %s (%s) has no reverse migration", step.From, step.To, step.Description))
			}
		}
		if len(blockers) == 0 {
			return nil
		}
		if best == nil || len(blockers) < len(best) {
			best = blockers
		}
	}
	return best
}
