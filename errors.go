package configmigrator

import "errors"

var (
	// ErrAlreadyAtTarget indicates that the document already has the requested version.
	ErrAlreadyAtTarget = errors.New("configuration is already at the target version")
	// ErrNoMigrationPath indicates that the registered steps do not connect two versions.
	ErrNoMigrationPath = errors.New("no migration path")
	// ErrAmbiguousPath indicates that more than one path exists and no explicit path was supplied.
	ErrAmbiguousPath = errors.New("ambiguous migration path")
	// ErrReverseUnsupported indicates that no registered path supports reverse migration.
	ErrReverseUnsupported = errors.New("reverse migration is not supported")
	// ErrInvalidStep indicates an invalid migration step registration.
	ErrInvalidStep = errors.New("invalid migration step")
	// ErrStalePreview indicates the input changed after a preview was computed.
	ErrStalePreview = errors.New("preview no longer matches the input")
	// ErrNotFound indicates that a configuration store does not contain a release yet.
	ErrNotFound = errors.New("configuration not found")
	// ErrGroupBlocked indicates that unresolved cross-file references prevent publishing the group.
	ErrGroupBlocked = errors.New("group migration is blocked")
	// ErrReleaseNotFound indicates that a historical release identifier is unknown.
	ErrReleaseNotFound = errors.New("release not found")
)
