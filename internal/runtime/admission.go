package runtime

import (
	"fmt"

	"github.com/fastschema/qjs"
)

// GroupVersionKind mirrors metav1.GroupVersionKind on the JS side.
type GroupVersionKind struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

// GroupVersionResource mirrors metav1.GroupVersionResource on the JS side.
type GroupVersionResource struct {
	Group    string `json:"group"`
	Version  string `json:"version"`
	Resource string `json:"resource"`
}

// UserInfo is the subset of authenticationv1.UserInfo we forward to JS.
type UserInfo struct {
	Username string              `json:"username,omitempty"`
	UID      string              `json:"uid,omitempty"`
	Groups   []string            `json:"groups,omitempty"`
	Extra    map[string][]string `json:"extra,omitempty"`
}

// AdmissionRequest is what JS validate(req)/mutate(req) sees. Mirrors the
// shape of admissionv1.AdmissionRequest, but with Object/OldObject already
// unmarshaled — JS gets real objects, not RawExtension byte slices.
//
// Object/OldObject stay map[string]any because the apiserver dispatches
// any GVK to a webhook (Pod, Deployment, custom CR, ...) and we don't want
// to commit the runtime to a fixed schema. Everything else is typed.
type AdmissionRequest struct {
	UID         string               `json:"uid"`
	Kind        GroupVersionKind     `json:"kind"`
	Resource    GroupVersionResource `json:"resource"`
	SubResource string               `json:"subResource,omitempty"`
	Name        string               `json:"name,omitempty"`
	Namespace   string               `json:"namespace,omitempty"`
	Operation   string               `json:"operation"`
	UserInfo    UserInfo             `json:"userInfo"`
	Object      map[string]any       `json:"object,omitempty"`
	OldObject   map[string]any       `json:"oldObject,omitempty"`
	DryRun      bool                 `json:"dryRun"`
}

// AdmissionResult is the decoded return value of a JS validate()/mutate()
// call. Mirrors the JS contract documented in the JSAdmission API doc.
type AdmissionResult struct {
	// Allowed mirrors AdmissionResponse.Allowed. JS that omits the field is
	// treated as denied (zero value), forcing policies to be explicit.
	Allowed bool `json:"allowed"`
	// Code lets JS set Status.Code on the AdmissionResponse (e.g. 403).
	Code int32 `json:"code,omitempty"`
	// Message is surfaced as Status.Message to the apiserver.
	Message string `json:"message,omitempty"`
	// Warnings are propagated as AdmissionResponse.Warnings.
	Warnings []string `json:"warnings,omitempty"`
	// ModifiedObject is honoured only for mutating policies. Validating
	// policies that return one trigger a controller-side warning and ignore
	// the field.
	ModifiedObject map[string]any `json:"modifiedObject,omitempty"`
}

// HandleAdmission invokes either validate(req) or mutate(req) on the
// persistent JS instance and returns the decoded result.
//
// The request is staged on globalThis as a real JS object via qjs.ToJsValue,
// then the export is called inside an IIFE — same pattern Handle() uses,
// which works reliably alongside host bindings.
//
// Caller MUST hold ManagedInstance.CallMu (qjs is not goroutine-safe).
func (i *Instance) HandleAdmission(req *AdmissionRequest, mutating bool) (*AdmissionResult, error) {
	if req == nil {
		return nil, fmt.Errorf("admission request is nil")
	}
	export := "validate"
	if mutating {
		export = "mutate"
	}
	ctx := i.rt.Context()

	jsReq, err := qjs.ToJsValue(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("convert request to JS: %w", err)
	}
	ctx.Global().SetPropertyStr("__admReq", jsReq)

	out, err := ctx.Eval("__adm_call__", qjs.Code(fmt.Sprintf(`(() => {
		if (typeof %[1]s !== "function") {
			throw new Error("admission policy does not export a %[1]s() function");
		}
		const r = %[1]s(globalThis.__admReq);
		globalThis.__admReq = undefined;
		return r;
	})()`, export)))
	if err != nil {
		return nil, fmt.Errorf("calling %s(): %w", export, err)
	}
	defer out.Free()

	if out.IsUndefined() || out.IsNull() {
		return nil, fmt.Errorf("%s() returned undefined — must return {allowed: bool, ...}", export)
	}
	result, err := qjs.JsObjectOrMapToGoStruct[AdmissionResult](out)
	if err != nil {
		return nil, fmt.Errorf("decode %s() return value: %w", export, err)
	}
	return &result, nil
}
