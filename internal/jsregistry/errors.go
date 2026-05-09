package jsregistry

import "errors"

// Build-stage sentinel errors. Wrapped with %w by Registry.build so callers
// can classify failures via errors.Is without sniffing message strings.
//
// The wrapped cause carries the verbose detail; the sentinel identifies
// which stage of the build broke. JSHook/JSAdmission reconcilers map each
// sentinel to a stable Event reason in conditions.ClassifyBuildError.
var (
	ErrNewVM      = errors.New("registry: new VM")
	ErrBindHost   = errors.New("registry: bind host")
	ErrLoadModule = errors.New("registry: load module")
	ErrPostBuild  = errors.New("registry: post-build")
	ErrUnknownKey = errors.New("registry: unknown key")
)

// MissingExportError is returned from a PostBuild hook when the loaded JS
// module is missing a required global function (config/handle for JSHook,
// validate/mutate for JSAdmission). The reconciler maps it to the
// EntrypointMissing event reason and surfaces Name() to users.
//
// This is its own type (not a sentinel) because the missing export name is
// part of the event message and the user-facing error.
type MissingExportError struct {
	Name string
}

func (e *MissingExportError) Error() string {
	return "missing required export: " + e.Name + "()"
}
