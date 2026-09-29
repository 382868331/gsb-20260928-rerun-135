package configmigrator

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
)

// Config is a versioned application configuration. The VersionKey field
// ("version" by default) stores the current configuration version.
type Config map[string]any

// Step mutates one configuration while moving it from FromVersion to
// ToVersion. A step may return an error to reject a configuration before it is
// published.
type Step struct {
	FromVersion string
	ToVersion   string
	Name        string
	Migrate     func(Config) error
}

func (s Step) key() string {
	return s.FromVersion + "\x00" + s.ToVersion
}

// Migrator contains a directed graph of version migration steps.
type Migrator struct {
	versionKey string
	steps      map[string]Step
}

// New creates a Migrator. Register additional migration steps as an
// application introduces newer configuration versions.
func New(steps ...Step) (*Migrator, error) {
	m := &Migrator{versionKey: "version", steps: make(map[string]Step)}
	for _, step := range steps {
		if err := m.Register(step); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// SetVersionKey changes the map key used to read and write a configuration's
// version. The empty key is rejected.
func (m *Migrator) SetVersionKey(key string) error {
	if key == "" {
		return errors.New("version key must not be empty")
	}
	m.versionKey = key
	return nil
}

// VersionKey returns the map key used for configuration versions.
func (m *Migrator) VersionKey() string {
	return m.versionKey
}

// Register adds or replaces one directed migration step.
func (m *Migrator) Register(step Step) error {
	if step.FromVersion == "" || step.ToVersion == "" {
		return errors.New("migration step versions must not be empty")
	}
	if step.FromVersion == step.ToVersion {
		return fmt.Errorf("migration step %q to %q must change version", step.FromVersion, step.ToVersion)
	}
	if step.Migrate == nil {
		return fmt.Errorf("migration step %q to %q requires a migrate function", step.FromVersion, step.ToVersion)
	}
	if step.Name == "" {
		step.Name = step.FromVersion + "->" + step.ToVersion
	}
	m.steps[step.key()] = step
	return nil
}

// PlanOptions controls migration path selection.
type PlanOptions struct {
	// Route explicitly selects a path when branches exist. It contains the
	// versions to move through after source and before target, for example
	// ["v2"] selects v1 -> v2 -> v3 rather than v1 -> v2-alt -> v3.
	Route []string
}

// Plan validates that exactly one migration path exists without executing any
// step. If multiple paths exist, opts.Route must identify one unambiguously.
func (m *Migrator) Plan(from, to string, opts *PlanOptions) ([]Step, error) {
	if from == "" || to == "" {
		return nil, errors.New("source and target versions must not be empty")
	}
	if from == to {
		return nil, nil
	}

	routes, _ := m.findRoutes(from, to)

	if len(routes) == 0 {
		return nil, fmt.Errorf("no migration path from %q to %q", from, to)
	}
	if opts != nil && len(opts.Route) > 0 {
		return selectRoute(routes, from, to, opts.Route)
	}
	if len(routes) == 1 {
		return routes[0], nil
	}
	return nil, ambiguousRouteError(from, to, routes)
}

func (m *Migrator) findRoutes(from, to string) ([][]Step, bool) {
	adjacency := make(map[string][]Step)
	for _, step := range m.steps {
		adjacency[step.FromVersion] = append(adjacency[step.FromVersion], step)
	}
	for versions := range adjacency {
		sort.Slice(adjacency[versions], func(i, j int) bool {
			left := adjacency[versions][i]
			right := adjacency[versions][j]
			if left.ToVersion != right.ToVersion {
				return left.ToVersion < right.ToVersion
			}
			return left.Name < right.Name
		})
	}

	var routes [][]Step
	var walk func(string, []Step, map[string]bool)
	walk = func(current string, path []Step, seen map[string]bool) {
		if current == to {
			routes = append(routes, append([]Step(nil), path...))
			return
		}
		for _, step := range adjacency[current] {
			if seen[step.ToVersion] {
				continue
			}
			seen[step.ToVersion] = true
			walk(step.ToVersion, append(path, step), seen)
			seen[step.ToVersion] = false
		}
	}
	walk(from, nil, map[string]bool{from: true})
	return routes, len(routes) > 0
}

func selectRoute(routes [][]Step, from, to string, wanted []string) ([]Step, error) {
	wantedVersions := make([]string, 0, len(wanted)+2)
	wantedVersions = append(wantedVersions, from)
	wantedVersions = append(wantedVersions, wanted...)
	wantedVersions = append(wantedVersions, to)

	var matches [][]Step
	for _, route := range routes {
		versions := make([]string, 0, len(route)+1)
		versions = append(versions, from)
		for _, step := range route {
			versions = append(versions, step.ToVersion)
		}
		if equalStrings(versions, wantedVersions) {
			matches = append(matches, route)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return nil, ambiguousRouteError(from, to, matches)
	}
	return nil, fmt.Errorf("requested route %v is not a migration path from %q to %q", wanted, from, to)
}

func ambiguousRouteError(from, to string, routes [][]Step) error {
	descriptions := make([][]string, 0, len(routes))
	for _, route := range routes {
		versions := []string{from}
		for _, step := range route {
			versions = append(versions, step.ToVersion)
		}
		descriptions = append(descriptions, versions)
	}
	return fmt.Errorf("migration from %q to %q is ambiguous; specify one route: %v", from, to, descriptions)
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

// StepRecord describes one executed or skipped migration step.
type StepRecord struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	Skipped     bool   `json:"skipped,omitempty"`
}

// Result reports a completed migration. A configuration already at the target
// version is left unchanged and Steps is empty.
type Result struct {
	SourceVersion string       `json:"source_version"`
	TargetVersion string       `json:"target_version"`
	Changed       bool         `json:"changed"`
	Steps         []StepRecord `json:"steps"`
}

// Migrate plans, executes, and validates an in-memory migration. The supplied
// config is not modified on failure. Steps must leave its version consistent
// after each successful call; the final version must equal target.
func (m *Migrator) Migrate(config Config, target string, opts *PlanOptions) (Result, error) {
	if config == nil {
		return Result{}, errors.New("configuration must not be nil")
	}
	current, _ := config[m.versionKey].(string)
	if current == "" {
		return Result{}, fmt.Errorf("configuration is missing string version key %q", m.versionKey)
	}
	steps, err := m.Plan(current, target, opts)
	if err != nil {
		return Result{}, err
	}
	if len(steps) == 0 {
		return Result{SourceVersion: current, TargetVersion: target}, nil
	}

	migrated := cloneConfig(config)
	records := make([]StepRecord, 0, len(steps))
	for i, step := range steps {
		if err := step.Migrate(migrated); err != nil {
			return Result{}, fmt.Errorf("migration step %s (%q to %q) failed: %w", step.Name, step.FromVersion, step.ToVersion, err)
		}
		got, _ := migrated[m.versionKey].(string)
		if got != step.ToVersion {
			return Result{}, fmt.Errorf("migration step %s reported version %q, want %q", step.Name, got, step.ToVersion)
		}
		records = append(records, StepRecord{
			Index:       i,
			Name:        step.Name,
			FromVersion: step.FromVersion,
			ToVersion:   step.ToVersion,
		})
	}

	for key, value := range migrated {
		config[key] = value
	}
	for key := range config {
		if _, ok := migrated[key]; !ok {
			delete(config, key)
		}
	}
	return Result{SourceVersion: current, TargetVersion: target, Changed: true, Steps: records}, nil
}

func cloneConfig(config Config) Config {
	clone := make(Config, len(config))
	for key, value := range config {
		clone[key] = deepCopy(value)
	}
	return clone
}

func deepCopy(value any) any {
	if value == nil {
		return nil
	}
	return deepCopyValue(reflect.ValueOf(value)).Interface()
}

func deepCopyValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return value
		}
		result := reflect.New(value.Elem().Type())
		result.Elem().Set(deepCopyValue(value.Elem()))
		return result
	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		return reflect.ValueOf(deepCopy(value.Interface()))
	case reflect.Slice:
		if value.IsNil() {
			return value
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Cap())
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(deepCopyValue(value.Index(i)))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(deepCopyValue(value.Index(i)))
		}
		return result
	case reflect.Map:
		if value.IsNil() {
			return value
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			result.SetMapIndex(iter.Key(), deepCopyValue(iter.Value()))
		}
		return result
	case reflect.Struct:
		result := value
		for i := 0; i < value.NumField(); i++ {
			if result.Field(i).CanSet() {
				result.Field(i).Set(deepCopyValue(value.Field(i)))
			}
		}
		return result
	default:
		return value
	}
}
