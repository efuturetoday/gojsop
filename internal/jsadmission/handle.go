package jsadmission

import (
	"context"
	"fmt"

	"github.com/fastschema/qjs"

	"github.com/o-haase/gojsop/internal/jsengine"
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

// Handle invokes either validate(req) or mutate(req) on the persistent JS VM
// and returns the decoded result.
//
// qjs.JsFuncToGo would have been even cleaner, but it only auto-converts
// primitive return types: for object returns it routes through toGoValue[any]
// (yielding map[string]any) and then reflect.Convert into the declared sample
// type — which fails for structs. So we Invoke and decode the *Value with
// JsObjectOrMapToGoStruct, which is the qjs-supported path for struct shapes.
//
// The request is converted to a real JS object via qjs.ToJsValue (carried by
// Value.Invoke) so Object/OldObject keep their structural identity for the
// policy code rather than going through a JSON roundtrip.
//
// Caller MUST hold the per-VM serialization lock — qjs is not goroutine-safe.
//
// ctx is plumbed into wazero via VM.WithContext; a context with deadline
// gives the JS call a real timeout (returns jsengine.ErrCancelled).
func Handle(ctx context.Context, vm *jsengine.VM, req *AdmissionRequest, mutating bool) (*AdmissionResult, error) {
	if req == nil {
		return nil, fmt.Errorf("admission request is nil")
	}
	export := "validate"
	if mutating {
		export = "mutate"
	}
	var result AdmissionResult
	err := vm.WithContext(ctx, func(c *qjs.Context) error {
		global := c.Global()
		fn := global.GetPropertyStr(export)
		defer fn.Free()
		if !fn.IsFunction() {
			return fmt.Errorf("admission policy does not export a %s() function", export)
		}

		out, err := global.Invoke(export, req)
		if err != nil {
			return fmt.Errorf("calling %s(): %w", export, jsengine.WrapEngineErr(ctx, err))
		}
		defer out.Free()

		if out.IsUndefined() || out.IsNull() {
			return fmt.Errorf("%s() returned undefined — must return {allowed: bool, ...}", export)
		}
		decoded, err := qjs.JsObjectOrMapToGoStruct[AdmissionResult](out)
		if err != nil {
			return fmt.Errorf("decode %s() return value: %w", export, err)
		}
		result = decoded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
