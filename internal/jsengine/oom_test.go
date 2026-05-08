package jsengine

import (
	"errors"
	"testing"
)

// TestWrapOOM pins the engine boundary translation: raw qjs OOM strings get
// lifted into ErrOOM, everything else passes through. Downstream code
// classifies via errors.Is — never by sniffing the message.
func TestWrapOOM(t *testing.T) {
	if wrapOOM(nil) != nil {
		t.Fatal("nil must pass through")
	}
	if got := wrapOOM(errOf("some other failure")); errors.Is(got, ErrOOM) {
		t.Fatal("unrelated error must not be wrapped as OOM")
	}
	// The exact wording qjs v0.0.6 emits — see TestMemoryLimit_Honoured.
	if got := wrapOOM(errOf("eval oom.js: InternalError: out of memory\n    at <eval> (oom.js:1:23)")); !errors.Is(got, ErrOOM) {
		t.Fatal("qjs OOM message must lift to ErrOOM")
	}
	// Case-insensitive — defensive against future qjs releases.
	if got := wrapOOM(errOf("Out Of Memory")); !errors.Is(got, ErrOOM) {
		t.Fatal("matching must be case-insensitive")
	}
}

func TestIsOOMError_OnlyMatchesSentinel(t *testing.T) {
	if IsOOMError(nil) {
		t.Fatal("nil must not be OOM")
	}
	// Raw OOM-shaped strings without the sentinel are NOT OOM. The whole
	// point of the typed sentinel is to refuse stringly-typed matches at
	// every layer except the engine boundary.
	if IsOOMError(errOf("out of memory")) {
		t.Fatal("unwrapped string must not be classified as OOM — only ErrOOM does")
	}
	if !IsOOMError(wrapOOM(errOf("out of memory"))) {
		t.Fatal("wrapped error must classify as OOM")
	}
}

type stringErr string

func (e stringErr) Error() string { return string(e) }
func errOf(s string) error        { return stringErr(s) }
