package jsengine

import "testing"

func TestIsOOMError(t *testing.T) {
	if IsOOMError(nil) {
		t.Fatal("nil error must not be classified as OOM")
	}
	if IsOOMError(errOf("some other failure")) {
		t.Fatal("unrelated error must not be classified as OOM")
	}
	// The exact wording qjs v0.0.6 emits — see TestMemoryLimit_Honoured.
	if !IsOOMError(errOf("eval oom.js: InternalError: out of memory\n    at <eval> (oom.js:1:23)")) {
		t.Fatal("qjs OOM message must be classified")
	}
	// Case-insensitive — defensive against future qjs releases.
	if !IsOOMError(errOf("Out Of Memory")) {
		t.Fatal("matching must be case-insensitive")
	}
}

type stringErr string

func (e stringErr) Error() string { return string(e) }
func errOf(s string) error        { return stringErr(s) }
