package jsengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrOOM is the typed sentinel for "qjs hit MemoryLimit". qjs itself
// surfaces OOM as an untyped JS InternalError whose message contains
// "out of memory" (verified against fastschema/qjs v0.0.6), so the string
// sniff lives in exactly one place — wrapEngineErr, called by
// CallExport/Eval — and downstream code (dispatcher rescue, future
// metrics) uses errors.Is(err, jsengine.ErrOOM) without ever touching
// err.Error().
var ErrOOM = errors.New("jsengine: out of memory")

// ErrCancelled is the typed sentinel for "qjs call was aborted by ctx
// cancellation / deadline". Surfaces when CloseOnContextDone tripped on
// the runtime's embedded context (see VM.withContext). Downstream code
// uses errors.Is(err, jsengine.ErrCancelled) to drive rescue / 504
// classification without sniffing wazero's error string.
var ErrCancelled = errors.New("jsengine: call cancelled")

// IsOOMError reports whether err is (or wraps) ErrOOM. Kept for callers
// that prefer the predicate; equivalent to errors.Is(err, ErrOOM).
func IsOOMError(err error) bool {
	return errors.Is(err, ErrOOM)
}

// wrapEngineErr lifts qjs's stringly-typed errors into typed sentinels at
// the engine boundary. Order matters: ctx cancellation is checked first
// because wazero may also surface it as "out of memory"-adjacent text in
// some cases, and a cancelled call should be classified as cancelled, not
// OOM. Non-engine errors and nil pass through unchanged.
//
// This is the only place in the codebase that sniffs raw qjs / wazero
// error messages.
func wrapEngineErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return fmt.Errorf("%w: %v", ErrCancelled, err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "out of memory") {
		return fmt.Errorf("%w: %v", ErrOOM, err)
	}
	return err
}
