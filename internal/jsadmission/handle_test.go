package jsadmission_test

import (
	"context"
	"strings"
	"testing"

	"github.com/o-haase/gojsop/internal/jsadmission"
	"github.com/o-haase/gojsop/internal/jsengine"
)

func newVMWithSource(t *testing.T, src string) *jsengine.VM {
	t.Helper()
	inst, err := jsengine.New(jsengine.Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)
	if err := inst.LoadModule(context.Background(), "policy.js", src); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	return inst
}

// jsadmission.R2
func TestHandle_Validate_Allow(t *testing.T) {
	inst := newVMWithSource(t, `
		function validate(req) {
			if (req.operation !== "CREATE") throw new Error("unexpected op");
			return { allowed: true };
		}
	`)
	res, err := jsadmission.Handle(context.Background(), inst, &jsadmission.AdmissionRequest{
		UID:       "abc",
		Operation: "CREATE",
		Object:    map[string]any{"kind": "Pod"},
	}, false)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed=true")
	}
}

// jsadmission.R5
func TestHandle_Validate_Deny(t *testing.T) {
	inst := newVMWithSource(t, `
		function validate(req) {
			return { allowed: false, message: "no", code: 403, warnings: ["w1"] };
		}
	`)
	res, err := jsadmission.Handle(context.Background(), inst, &jsadmission.AdmissionRequest{}, false)
	if err != nil {
		t.Fatalf("Handle: %v", err)
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

func TestHandle_Mutate_AddLabel(t *testing.T) {
	inst := newVMWithSource(t, `
		function mutate(req) {
			const obj = req.object;
			obj.metadata = obj.metadata || {};
			obj.metadata.labels = obj.metadata.labels || {};
			obj.metadata.labels.team = "frontend";
			return { allowed: true, modifiedObject: obj };
		}
	`)
	res, err := jsadmission.Handle(context.Background(), inst, &jsadmission.AdmissionRequest{
		Object: map[string]any{
			"metadata": map[string]any{"name": "p"},
		},
	}, true)
	if err != nil {
		t.Fatalf("Handle: %v", err)
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

// jsadmission.R2
func TestHandle_MissingExport(t *testing.T) {
	inst := newVMWithSource(t, `function validate(req) { return {allowed:true}; }`)
	_, err := jsadmission.Handle(context.Background(), inst, &jsadmission.AdmissionRequest{}, true)
	if err == nil {
		t.Fatal("expected error when mutate() is missing")
	}
	if !strings.Contains(err.Error(), "mutate") {
		t.Fatalf("error should mention missing mutate(): %v", err)
	}
}

func TestHandle_ThrowsSurface(t *testing.T) {
	inst := newVMWithSource(t, `function validate(req) { throw new Error("boom"); }`)
	_, err := jsadmission.Handle(context.Background(), inst, &jsadmission.AdmissionRequest{}, false)
	if err == nil {
		t.Fatal("expected error from JS throw")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should propagate JS message: %v", err)
	}
}
