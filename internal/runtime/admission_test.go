package runtime

import (
	"strings"
	"testing"
)

func newInstanceWithSource(t *testing.T, src string) *Instance {
	t.Helper()
	inst, err := New(Resources{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)
	if err := inst.LoadModule("policy.js", src); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	return inst
}

func TestHandleAdmission_Validate_Allow(t *testing.T) {
	inst := newInstanceWithSource(t, `
		function validate(req) {
			if (req.operation !== "CREATE") throw new Error("unexpected op");
			return { allowed: true };
		}
	`)
	res, err := inst.HandleAdmission(&AdmissionRequest{
		UID:       "abc",
		Operation: "CREATE",
		Object:    map[string]any{"kind": "Pod"},
	}, false)
	if err != nil {
		t.Fatalf("HandleAdmission: %v", err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed=true")
	}
}

func TestHandleAdmission_Validate_Deny(t *testing.T) {
	inst := newInstanceWithSource(t, `
		function validate(req) {
			return { allowed: false, message: "no", code: 403, warnings: ["w1"] };
		}
	`)
	res, err := inst.HandleAdmission(&AdmissionRequest{}, false)
	if err != nil {
		t.Fatalf("HandleAdmission: %v", err)
	}
	if res.Allowed {
		t.Fatal("expected allowed=false")
	}
	if res.Message != "no" || res.Code != 403 {
		t.Fatalf("message/code: got %+v", res)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != "w1" {
		t.Fatalf("warnings: got %v", res.Warnings)
	}
}

func TestHandleAdmission_Mutate_AddLabel(t *testing.T) {
	inst := newInstanceWithSource(t, `
		function mutate(req) {
			const obj = req.object;
			obj.metadata = obj.metadata || {};
			obj.metadata.labels = obj.metadata.labels || {};
			obj.metadata.labels.team = "frontend";
			return { allowed: true, modifiedObject: obj };
		}
	`)
	res, err := inst.HandleAdmission(&AdmissionRequest{
		Object: map[string]any{
			"metadata": map[string]any{"name": "p"},
		},
	}, true)
	if err != nil {
		t.Fatalf("HandleAdmission: %v", err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed=true")
	}
	meta, _ := res.ModifiedObject["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	if labels["team"] != "frontend" {
		t.Fatalf("team label: got %v", labels)
	}
}

func TestHandleAdmission_MissingExport(t *testing.T) {
	inst := newInstanceWithSource(t, `function validate(req) { return {allowed:true}; }`)
	_, err := inst.HandleAdmission(&AdmissionRequest{}, true)
	if err == nil {
		t.Fatal("expected error when mutate() is missing")
	}
	if !strings.Contains(err.Error(), "mutate") {
		t.Fatalf("error should mention missing mutate(): %v", err)
	}
}

func TestHandleAdmission_ThrowsSurface(t *testing.T) {
	inst := newInstanceWithSource(t, `function validate(req) { throw new Error("boom"); }`)
	_, err := inst.HandleAdmission(&AdmissionRequest{}, false)
	if err == nil {
		t.Fatal("expected error from JS throw")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should propagate JS message: %v", err)
	}
}
