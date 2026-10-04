package jsadmission

import "testing"

// jsadmission.R6
func TestCreatePatch_NoChange(t *testing.T) {
	original := []byte(`{"metadata":{"name":"p"}}`)
	modified := map[string]any{"metadata": map[string]any{"name": "p"}}
	ops, err := CreatePatch(original, modified)
	if err != nil {
		t.Fatalf("CreatePatch: %v", err)
	}
	if ops != nil {
		t.Fatalf("expected no ops, got %+v", ops)
	}
}

// jsadmission.R6
func TestCreatePatch_FiltersImmutable(t *testing.T) {
	original := []byte(`{"metadata":{"name":"p","uid":"abc","creationTimestamp":"2026-05-07T00:00:00Z"},"status":{"phase":"Pending"}}`)
	modified := map[string]any{
		"metadata": map[string]any{
			"name":              "p",
			"uid":               "different",
			"creationTimestamp": "2026-05-08T00:00:00Z",
			"labels":            map[string]any{"team": "frontend"},
		},
		"status": map[string]any{"phase": "Running"},
	}
	ops, err := CreatePatch(original, modified)
	if err != nil {
		t.Fatalf("CreatePatch: %v", err)
	}
	for _, op := range ops {
		if op.Path == "/metadata/uid" || op.Path == "/metadata/creationTimestamp" || op.Path == "/status/phase" {
			t.Fatalf("immutable path leaked through filter: %+v", op)
		}
	}
	// Only the label addition should survive.
	if len(ops) != 1 || ops[0].Path != pathLabels {
		t.Fatalf("expected only labels add, got %+v", ops)
	}
}
