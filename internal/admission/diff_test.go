package admission

import "testing"

func TestCreatePatch_AddLabel(t *testing.T) {
	original := []byte(`{"metadata":{"name":"p"}}`)
	modified := map[string]any{
		"metadata": map[string]any{
			"name":   "p",
			"labels": map[string]any{"team": "frontend"},
		},
	}
	ops, err := CreatePatch(original, modified)
	if err != nil {
		t.Fatalf("CreatePatch: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d: %+v", len(ops), ops)
	}
	if ops[0].Operation != "add" || ops[0].Path != "/metadata/labels" {
		t.Fatalf("unexpected op: %+v", ops[0])
	}
}

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
	if len(ops) != 1 || ops[0].Path != "/metadata/labels" {
		t.Fatalf("expected only labels add, got %+v", ops)
	}
}
