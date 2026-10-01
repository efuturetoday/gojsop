package jsengine

import (
	"errors"
	"fmt"
	"strings"
)

// ErrOOM is the typed sentinel for "the script hit its memory limit"
// (JS_SetMemoryLimit). The VM stays usable. Callers use errors.Is, never the
// message (js-execution.R4).
var ErrOOM = errors.New("jsengine: out of memory")

// ErrCancelled is the typed sentinel for "the call was stopped because its
// context ended" (deadline or cancellation). The QuickJS interrupt handler
// stopped the script; the VM stays usable. The error also wraps the cause of
// the context (context.DeadlineExceeded or context.Canceled).
var ErrCancelled = errors.New("jsengine: call cancelled")

// ErrClosed is returned for a call on a VM that was closed.
var ErrClosed = errors.New("jsengine: vm closed")

// IsOOMError reports whether err is (or wraps) ErrOOM.
func IsOOMError(err error) bool {
	return errors.Is(err, ErrOOM)
}

// TrapError is a failure of the wasm module itself: a trap, a panic of a host
// function, or any other error that is not an exception of the script. The
// state of the module is unknown afterwards, so the VM is marked broken
// (VM.Usable reports false) and its owner must replace it.
type TrapError struct{ Err error }

func (e *TrapError) Error() string { return "jsengine: wasm trap: " + e.Err.Error() }
func (e *TrapError) Unwrap() error { return e.Err }

// JSError is an exception thrown by the script. Message is the one line form
// ("TypeError: x is not a function"), Stack the QuickJS stack with file and
// line ("    at handle (hook.js:3:9)"); Stack is empty when the script threw
// something that is no Error object. Error() carries both, so a log line or
// an event shows where the script failed (EXEC-4).
type JSError struct {
	Message string
	Stack   string
}

func (e *JSError) Error() string {
	if e.Stack == "" {
		return e.Message
	}
	return e.Message + "\n" + strings.TrimRight(e.Stack, "\n")
}

func parseJSError(out []byte) *JSError {
	msg, stack, _ := strings.Cut(string(out), "\n")
	return &JSError{Message: msg, Stack: stack}
}

func cancelled(cause, ctxErr error) error {
	if ctxErr != nil {
		return fmt.Errorf("%w: %w", ErrCancelled, ctxErr)
	}
	return fmt.Errorf("%w: %v", ErrCancelled, cause)
}
