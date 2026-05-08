package jsengine

import (
	"errors"
	"fmt"
	"strings"
)

// ErrOOM is the typed sentinel for "qjs hit MemoryLimit". qjs itself
// surfaces OOM as an untyped JS InternalError whose message contains
// "out of memory" (verified against fastschema/qjs v0.0.6), so the string
// sniff lives in exactly one place — wrapOOM, called by CallExport/Eval —
// and downstream code (dispatcher rescue, future metrics) uses
// errors.Is(err, jsengine.ErrOOM) without ever touching err.Error().
var ErrOOM = errors.New("jsengine: out of memory")

// IsOOMError reports whether err is (or wraps) ErrOOM. Kept for callers
// that prefer the predicate; equivalent to errors.Is(err, ErrOOM).
func IsOOMError(err error) bool {
	return errors.Is(err, ErrOOM)
}

// wrapOOM lifts qjs's stringly-typed OOM error into ErrOOM at the engine
// boundary. Non-OOM errors and nil pass through unchanged. This is the
// only place in the codebase that sniffs the raw qjs message.
func wrapOOM(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "out of memory") {
		return fmt.Errorf("%w: %v", ErrOOM, err)
	}
	return err
}
