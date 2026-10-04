package jsadmission_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/o-haase/gojsop/internal/jsadmission"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsregistry/registrytest"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// loadPolicy loads src as the policy of a fresh key in a real registry and
// returns the Runner and the key.
func loadPolicy(t *testing.T, src string) (jsrun.Runner, jsrun.Key) {
	t.Helper()
	reg := jsregistry.NewRegistry()
	key := jsrun.AdmissionKey(types.NamespacedName{Name: "policy"})
	t.Cleanup(func() { reg.Drop(key) })
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, jsrun.Spec{Source: []byte(src), SourceHash: "h"}); err != nil {
		t.Fatalf("load policy: %v", err)
	}
	return reg, key
}

// jsadmission.R2
func TestHandle_Validate_Allow(t *testing.T) {
	rt, key := loadPolicy(t, `
		function validate(req) {
			if (req.operation !== "CREATE") throw new Error("unexpected op");
			return { allowed: true };
		}
	`)
	res, hres, err := jsadmission.Handle(context.Background(), rt, key, &jsadmission.AdmissionRequest{
		UID:       "abc",
		Operation: "CREATE",
		Object:    map[string]any{"kind": "Pod"},
	}, false)
	if err != nil || hres.Err != nil {
		t.Fatalf("Handle: %v, %v", err, hres.Err)
	}
	if !res.Allowed {
		t.Fatal("expected allowed=true")
	}
}

// jsadmission.R5
func TestHandle_Validate_Deny(t *testing.T) {
	rt, key := loadPolicy(t, `
		function validate(req) {
			return { allowed: false, message: "no", code: 403, warnings: ["w1"] };
		}
	`)
	res, hres, err := jsadmission.Handle(context.Background(), rt, key, &jsadmission.AdmissionRequest{}, false)
	if err != nil || hres.Err != nil {
		t.Fatalf("Handle: %v, %v", err, hres.Err)
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

// jsadmission.R2
func TestHandle_MissingExport(t *testing.T) {
	rt, key := loadPolicy(t, `function validate(req) { return {allowed:true}; }`)
	_, hres, err := jsadmission.Handle(context.Background(), rt, key, &jsadmission.AdmissionRequest{}, true)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if hres.Err == nil {
		t.Fatal("expected error when mutate() is missing")
	}
	if !strings.Contains(hres.Err.Error(), "mutate") {
		t.Fatalf("error should mention missing mutate(): %v", hres.Err)
	}
}
