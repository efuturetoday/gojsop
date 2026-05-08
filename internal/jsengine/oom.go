package jsengine

import "strings"

// IsOOMError reports whether err comes from qjs hitting MemoryLimit. qjs
// surfaces it as a JS InternalError whose message contains "out of memory"
// (verified against fastschema/qjs v0.0.6 in TestMemoryLimit_Honoured).
func IsOOMError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "out of memory")
}
