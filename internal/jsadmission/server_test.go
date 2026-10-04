package jsadmission

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	logr "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsregistry/registrytest"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// pathLabels is the JSON-patch path the diff tests expect for label changes.
const pathLabels = "/metadata/labels"

// loadPolicy starts a real engine instance with the given JS source and parks
// it in a fresh registry under `key`. Mirrors what the reconciler does.
func loadPolicy(t *testing.T, src string, key types.NamespacedName) *jsregistry.Registry {
	t.Helper()
	reg := jsregistry.NewRegistry()
	t.Cleanup(func() { reg.Drop(jsrun.AdmissionKey(key)) })
	source := []byte(src)
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), jsrun.AdmissionKey(key), jsrun.Spec{Source: source, SourceHash: "h1"}); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	return reg
}

func postReview(t *testing.T, h http.Handler, path string, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	t.Helper()
	in := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "AdmissionReview"},
		Request:  req,
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v\n%s", err, w.Body.String())
	}
	if out.Response == nil {
		t.Fatal("nil response")
	}
	return out.Response
}

// jsadmission.R14
func TestServer_Validate_AllowedRoundtrip(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "policy-a"}
	reg := loadPolicy(t, `function validate(req){ return {allowed: true}; }`, key)
	srv := NewServer(reg, logr.Log)
	srv.Register(PolicyEntry{Key: key, Mutating: false, Timeout: 2 * time.Second})

	resp := postReview(t, srv.ValidateHandler(), PathFor(key, false), &admissionv1.AdmissionRequest{
		UID:       "abc",
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: []byte(`{"kind":"Pod"}`)},
	})
	if string(resp.UID) != "abc" {
		t.Fatalf("UID echo: got %q", resp.UID)
	}
	if !resp.Allowed {
		t.Fatalf("expected allowed=true: %+v", resp)
	}
}

// jsadmission.R9
func TestServer_Validate_FailurePolicy_Fail_OnJSThrow(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "policy-fail"}
	reg := loadPolicy(t, `function validate(req){ throw new Error("boom"); }`, key)
	srv := NewServer(reg, logr.Log)
	srv.Register(PolicyEntry{Key: key, FailurePolicy: admissionregv1.Fail, Timeout: 2 * time.Second})

	resp := postReview(t, srv.ValidateHandler(), PathFor(key, false), &admissionv1.AdmissionRequest{UID: "u"})
	if resp.Allowed {
		t.Fatal("Fail policy must reject JS errors")
	}
	if resp.Result == nil || !strings.Contains(resp.Result.Message, "boom") {
		t.Fatalf("error message not propagated: %+v", resp.Result)
	}
}

// jsadmission.R9
func TestServer_Validate_FailurePolicy_Ignore_OnJSThrow(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "policy-ignore"}
	reg := loadPolicy(t, `function validate(req){ throw new Error("boom"); }`, key)
	srv := NewServer(reg, logr.Log)
	srv.Register(PolicyEntry{Key: key, FailurePolicy: admissionregv1.Ignore, Timeout: 2 * time.Second})

	resp := postReview(t, srv.ValidateHandler(), PathFor(key, false), &admissionv1.AdmissionRequest{UID: "u"})
	if !resp.Allowed {
		t.Fatal("Ignore policy must allow on JS errors")
	}
}

// jsadmission.R6
func TestServer_Mutate_AddsLabel_AsJSONPatch(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "policy-mutate"}
	src := `
		function mutate(req) {
			const obj = req.object;
			obj.metadata = obj.metadata || {};
			obj.metadata.labels = obj.metadata.labels || {};
			obj.metadata.labels.team = "frontend";
			return { allowed: true, modifiedObject: obj };
		}`
	reg := loadPolicy(t, src, key)
	srv := NewServer(reg, logr.Log)
	srv.Register(PolicyEntry{Key: key, Mutating: true, Timeout: 2 * time.Second})

	original := []byte(`{"metadata":{"name":"p"}}`)
	resp := postReview(t, srv.MutateHandler(), PathFor(key, true), &admissionv1.AdmissionRequest{
		UID:       "m",
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: original},
	})
	if !resp.Allowed {
		t.Fatal("expected allowed=true")
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Fatalf("patchType: got %v", resp.PatchType)
	}
	var ops []map[string]any
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if len(ops) != 1 || ops[0]["op"] != "add" || ops[0]["path"] != pathLabels {
		t.Fatalf("unexpected patch: %+v", ops)
	}
}

// jsadmission.R1
func TestServer_UnknownPolicy_Returns404(t *testing.T) {
	srv := NewServer(jsregistry.NewRegistry(), logr.Log)
	r := httptest.NewRequest(http.MethodPost, PathFor(types.NamespacedName{Namespace: "default", Name: "ghost"}, false), bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	srv.ValidateHandler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}
}

// captureEvents builds an EventEmitter that pushes (reason, message) tuples
// onto a channel. Bigger buffer than strictly needed so a panic-then-failure
// path that emits twice doesn't deadlock the test goroutine.
func captureEvents(buf int) (chan [2]string, EventEmitter) {
	ch := make(chan [2]string, buf)
	return ch, func(_, reason, message string) {
		ch <- [2]string{reason, message}
	}
}

// TestServer_Emits_ReviewFailed_OnJSThrow asserts that when validate() throws
// a regular JS Error, the server publishes ReviewFailed with the static
// message — never the JS error string (which would defeat dedup at scale).
//
// jsadmission.R21
func TestServer_Emits_ReviewFailed_OnJSThrow(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "policy-evt-fail"}
	reg := loadPolicy(t, `function validate(req){ throw new Error("very specific message"); }`, key)
	srv := NewServer(reg, logr.Log)

	events, emit := captureEvents(4)
	srv.Register(PolicyEntry{Key: key, FailurePolicy: admissionregv1.Fail, Timeout: 2 * time.Second, Emit: emit})

	postReview(t, srv.ValidateHandler(), PathFor(key, false), &admissionv1.AdmissionRequest{UID: "u"})

	select {
	case ev := <-events:
		if ev[0] != "ReviewFailed" {
			t.Fatalf("event reason: got %q want ReviewFailed", ev[0])
		}
		if ev[1] != "review returned an error" {
			t.Fatalf("event message must be the static template, got %q (the JS error string would explode dedup cardinality)", ev[1])
		}
		if strings.Contains(ev[1], "very specific message") {
			t.Fatal("JS error string leaked into event message — must stay in conditions/logs only")
		}
	case <-time.After(time.Second):
		t.Fatal("expected a ReviewFailed event, none arrived")
	}
}

// jsadmission.R16
func TestServer_Mutate_Denied_HasNoPatch(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "policy-deny-mutate"}
	src := `
		function mutate(req) {
			const obj = req.object;
			obj.metadata.labels = { team: "frontend" };
			return { allowed: false, message: "no", modifiedObject: obj };
		}`
	reg := loadPolicy(t, src, key)
	srv := NewServer(reg, logr.Log)
	srv.Register(PolicyEntry{Key: key, Mutating: true, Timeout: 2 * time.Second})

	resp := postReview(t, srv.MutateHandler(), PathFor(key, true), &admissionv1.AdmissionRequest{
		UID:       "d",
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: []byte(`{"metadata":{"name":"p"}}`)},
	})
	if resp.Allowed {
		t.Fatal("expected allowed=false")
	}
	if resp.Patch != nil || resp.PatchType != nil {
		t.Fatalf("denied response carries a patch: %s (%v)", resp.Patch, resp.PatchType)
	}
}

// The script sees every field of the request, object and oldObject as real
// objects.
//
// jsadmission.R3
func TestServer_PassesEveryRequestFieldToScript(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "policy-echo"}
	reg := loadPolicy(t, `function validate(req){ return {allowed: true, message: JSON.stringify(req)}; }`, key)
	srv := NewServer(reg, logr.Log)
	srv.Register(PolicyEntry{Key: key, Timeout: 2 * time.Second})

	dryRun := true
	resp := postReview(t, srv.ValidateHandler(), PathFor(key, false), &admissionv1.AdmissionRequest{
		UID:         "uid-1",
		Kind:        metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
		Resource:    metav1.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		SubResource: "scale",
		Name:        "web",
		Namespace:   "shop",
		Operation:   admissionv1.Update,
		UserInfo: authenticationv1.UserInfo{
			Username: "alice", UID: "u-alice", Groups: []string{"dev"},
			Extra: map[string]authenticationv1.ExtraValue{"team": {"a"}},
		},
		Object:    runtime.RawExtension{Raw: []byte(`{"metadata":{"name":"web"},"spec":{"replicas":3}}`)},
		OldObject: runtime.RawExtension{Raw: []byte(`{"metadata":{"name":"web"},"spec":{"replicas":1}}`)},
		DryRun:    &dryRun,
	})
	if resp.Result == nil {
		t.Fatalf("no message: %+v", resp)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(resp.Result.Message), &got); err != nil {
		t.Fatalf("decode echoed req: %v\n%s", err, resp.Result.Message)
	}
	want := map[string]any{
		"uid":         "uid-1",
		"kind":        map[string]any{"group": "apps", "version": "v1", "kind": "Deployment"},
		"resource":    map[string]any{"group": "apps", "version": "v1", "resource": "deployments"},
		"subResource": "scale",
		"name":        "web",
		"namespace":   "shop",
		"operation":   "UPDATE",
		"userInfo": map[string]any{
			"username": "alice", "uid": "u-alice", "groups": []any{"dev"},
			"extra": map[string]any{"team": []any{"a"}},
		},
		"object":    map[string]any{"metadata": map[string]any{"name": "web"}, "spec": map[string]any{"replicas": float64(3)}},
		"oldObject": map[string]any{"metadata": map[string]any{"name": "web"}, "spec": map[string]any{"replicas": float64(1)}},
		"dryRun":    true,
	}
	for field, w := range want {
		g, ok := got[field]
		if !ok {
			t.Errorf("field %q missing in req", field)
			continue
		}
		gb, _ := json.Marshal(g)
		wb, _ := json.Marshal(w)
		if string(gb) != string(wb) {
			t.Errorf("field %q: got %s, want %s", field, gb, wb)
		}
	}
}

// A result without `allowed` denies; a result of undefined or null is a
// script failure and goes to failurePolicy (Ignore admits it with a warning,
// which a plain deny never does).
//
// jsadmission.R4
func TestServer_MissingAllowedDenies_NullOrUndefinedFails(t *testing.T) {
	t.Run("omitted allowed denies", func(t *testing.T) {
		key := types.NamespacedName{Namespace: "default", Name: "policy-no-allowed"}
		reg := loadPolicy(t, `function validate(req){ return {message: "forgot"}; }`, key)
		srv := NewServer(reg, logr.Log)
		srv.Register(PolicyEntry{Key: key, FailurePolicy: admissionregv1.Ignore, Timeout: 2 * time.Second})

		resp := postReview(t, srv.ValidateHandler(), PathFor(key, false), &admissionv1.AdmissionRequest{UID: "u"})
		if resp.Allowed {
			t.Fatalf("omitted allowed must deny: %+v", resp)
		}
		if resp.Result == nil || resp.Result.Message != "forgot" {
			t.Fatalf("message not passed through: %+v", resp.Result)
		}
	})
	for _, tc := range []struct{ name, body string }{
		{"undefined", `function validate(req){ }`},
		{"null", `function validate(req){ return null; }`},
	} {
		t.Run(tc.name+" is a script failure", func(t *testing.T) {
			key := types.NamespacedName{Namespace: "default", Name: "policy-" + tc.name}
			reg := loadPolicy(t, tc.body, key)
			srv := NewServer(reg, logr.Log)
			srv.Register(PolicyEntry{Key: key, FailurePolicy: admissionregv1.Ignore, Timeout: 2 * time.Second})

			resp := postReview(t, srv.ValidateHandler(), PathFor(key, false), &admissionv1.AdmissionRequest{UID: "u"})
			if !resp.Allowed {
				t.Fatalf("failurePolicy Ignore must admit a script failure: %+v", resp)
			}
			if len(resp.Warnings) == 0 || !strings.HasPrefix(resp.Warnings[0], "gojsop: ") {
				t.Fatalf("no failurePolicy warning: %v", resp.Warnings)
			}
		})
	}
}

// jsadmission.R7
func TestServer_Validate_IgnoresModifiedObject(t *testing.T) {
	key := types.NamespacedName{Namespace: "default", Name: "policy-validate-mod"}
	src := `
		function validate(req) {
			const obj = req.object;
			obj.metadata.labels = { team: "frontend" };
			return { allowed: true, modifiedObject: obj };
		}`
	reg := loadPolicy(t, src, key)
	srv := NewServer(reg, logr.Log)
	srv.Register(PolicyEntry{Key: key, Timeout: 2 * time.Second})

	resp := postReview(t, srv.ValidateHandler(), PathFor(key, false), &admissionv1.AdmissionRequest{
		UID:       "v",
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: []byte(`{"metadata":{"name":"p"}}`)},
	})
	if !resp.Allowed {
		t.Fatal("expected allowed=true")
	}
	if resp.Patch != nil || resp.PatchType != nil {
		t.Fatalf("validating policy produced a patch: %s (%v)", resp.Patch, resp.PatchType)
	}
}

// While the policy has no VM (a build runs or failed) the request is decided by
// failurePolicy at once; it does not wait for the build.
//
// jsadmission.R19
func TestServer_NoVM_AppliesFailurePolicyAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  admissionregv1.FailurePolicyType
		allowed bool
	}{
		{"Fail denies", admissionregv1.Fail, false},
		{"Ignore allows", admissionregv1.Ignore, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := types.NamespacedName{Name: "policy-building"}
			reg := jsregistry.NewRegistry()
			regKey := jsrun.AdmissionKey(key)
			t.Cleanup(func() { reg.Drop(regKey) })
			// A top-level endless loop: the build runs until the default 30 s
			// deadline, the key stays Building for the whole test.
			if st := reg.Ensure(regKey, jsrun.Spec{Source: []byte(`while(true){}`), SourceHash: "h"}); st.Phase != jsrun.PhasePreparing {
				t.Fatalf("Ensure: %v, want Building", st.Phase)
			}
			srv := NewServer(reg, logr.Log)
			srv.Register(PolicyEntry{Key: key, FailurePolicy: tc.policy, Timeout: 5 * time.Second})

			start := time.Now()
			resp := postReview(t, srv.ValidateHandler(), PathFor(key, false), &admissionv1.AdmissionRequest{UID: "u"})
			if time.Since(start) > time.Second {
				t.Fatalf("review took %v: it waited for the build", time.Since(start))
			}
			if resp.Allowed != tc.allowed {
				t.Fatalf("allowed = %v, want %v (failurePolicy %s)", resp.Allowed, tc.allowed, tc.policy)
			}
		})
	}
}
