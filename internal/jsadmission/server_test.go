package jsadmission

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	logr "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsregistry"
)

// loadPolicy starts a real qjs instance with the given JS source and parks
// it in a fresh registry under `key`. Mirrors what the reconciler does.
func loadPolicy(t *testing.T, src string, key types.NamespacedName) *jsregistry.Registry {
	t.Helper()
	reg := jsregistry.NewRegistry()
	t.Cleanup(func() { reg.Drop(key) })
	source := []byte(src)
	// Embed a no-op config() so Registry.GetOrLoad doesn't reject the source.
	if !strings.Contains(src, "function config(") {
		source = append([]byte("function config(){return {configVersion:'v1'}}\n"), source...)
	}
	if _, _, err := reg.GetOrLoad(key, source, "h1", jsengine.Limits{}, nil); err != nil {
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
	if len(ops) != 1 || ops[0]["op"] != "add" || ops[0]["path"] != "/metadata/labels" {
		t.Fatalf("unexpected patch: %+v", ops)
	}
}

func TestServer_UnknownPolicy_Returns404(t *testing.T) {
	srv := NewServer(jsregistry.NewRegistry(), logr.Log)
	r := httptest.NewRequest(http.MethodPost, PathFor(types.NamespacedName{Namespace: "default", Name: "ghost"}, false), bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	srv.ValidateHandler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}
}

func TestPathFor_RoundTrip(t *testing.T) {
	cases := []types.NamespacedName{
		{Namespace: "default", Name: "p"},
		{Namespace: "", Name: "cluster-policy"},
	}
	for _, k := range cases {
		got, ok := keyFromPath(PathFor(k, false), PathPrefixValidate)
		if !ok {
			t.Fatalf("parse failed for %v", k)
		}
		if got != k {
			t.Fatalf("roundtrip: %v != %v", got, k)
		}
	}
}
