package jsadmission

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/o-haase/gojsop/internal/jsrun"
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

// Handle invokes either validate(req) or mutate(req) on the script of key and
// returns the decoded result. The Result classifies how the call ended; the
// error is set only when nothing ran (jsrun.ErrUnknownKey,
// jsrun.ErrVMUnavailable) or the request is nil. A return value that is
// undefined, null or not of the result shape turns an OK call into
// jsrun.OutcomeError.
//
// The request travels as the one argument of the export; the Runner converts
// it, so Object/OldObject keep their structural identity for the policy code.
// Locking, panic recovery and the deadline of ctx belong to the Runner.
// jsadmission.R2
// jsadmission.R4
func Handle(ctx context.Context, rt jsrun.Runner, key jsrun.Key, req *AdmissionRequest, mutating bool) (*AdmissionResult, jsrun.Result, error) {
	if req == nil {
		return nil, jsrun.Result{Outcome: jsrun.OutcomeError, Err: fmt.Errorf("admission request is nil")}, nil
	}
	export := "validate"
	if mutating {
		export = "mutate"
	}
	var raw json.RawMessage
	res, err := rt.Invoke(ctx, key, export, req, &raw)
	if err != nil || res.Outcome != jsrun.OutcomeOK {
		return nil, res, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		res.Outcome = jsrun.OutcomeError
		res.Err = fmt.Errorf("%s() returned undefined — must return {allowed: bool, ...}", export)
		return nil, res, nil
	}
	var result AdmissionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		res.Outcome = jsrun.OutcomeError
		res.Err = fmt.Errorf("decode %s() return value: %w", export, err)
		return nil, res, nil
	}
	return &result, res, nil
}
