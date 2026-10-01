// Package jsadmission hosts the HTTP webhook server, the JSONPatch diffing
// helper, and the central VWC/MWC registrar for JSAdmission policies, plus
// the JS-side admission request/result contract.
package jsadmission

import (
	"encoding/json"
	"fmt"
	"strings"

	"gomodules.xyz/jsonpatch/v2"
)

// immutablePatchPrefixes are paths the apiserver rejects when present in a
// MutatingWebhook patch. The auto-diff pipeline always strips them, so JS
// code that accidentally returns an unmodified copy of req.object doesn't
// produce a patch the apiserver will reject.
var immutablePatchPrefixes = []string{
	"/metadata/uid",
	"/metadata/resourceVersion",
	"/metadata/generation",
	"/metadata/creationTimestamp",
	"/metadata/deletionTimestamp",
	"/metadata/deletionGracePeriodSeconds",
	"/metadata/selfLink",
	"/metadata/managedFields",
	"/status",
}

// CreatePatch diffs originalRaw → modifiedObject and returns a JSONPatch
// (RFC 6902) suitable for an AdmissionResponse.Patch. Immutable paths are
// filtered out (see immutablePatchPrefixes).
// jsadmission.R6
func CreatePatch(originalRaw []byte, modifiedObject map[string]any) ([]jsonpatch.JsonPatchOperation, error) {
	if len(originalRaw) == 0 || modifiedObject == nil {
		return nil, nil
	}
	modifiedRaw, err := json.Marshal(modifiedObject)
	if err != nil {
		return nil, fmt.Errorf("marshal modifiedObject: %w", err)
	}
	ops, err := jsonpatch.CreatePatch(originalRaw, modifiedRaw)
	if err != nil {
		return nil, fmt.Errorf("create patch: %w", err)
	}
	return filterImmutable(ops), nil
}

// filterImmutable drops operations whose path matches any immutable prefix.
// Returns nil if all ops were filtered out (so the caller emits no patch).
func filterImmutable(ops []jsonpatch.JsonPatchOperation) []jsonpatch.JsonPatchOperation {
	out := ops[:0]
	for _, op := range ops {
		if isImmutable(op.Path) {
			continue
		}
		out = append(out, op)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isImmutable(path string) bool {
	for _, p := range immutablePatchPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
