package jsadmission

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"

	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jslifecycle"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// EventEmitter is the shared lifecycle-event callback. Aliased from
// jslifecycle so existing callers (jsadmission.EventEmitter) keep working
// while the type lives in one place.
type EventEmitter = jslifecycle.EventEmitter

// PathPrefixValidate / PathPrefixMutate are the URL prefixes the admission
// server exposes. Each policy lives at <prefix>{namespace}/{name}; for
// cluster-scoped JSAdmissions the namespace segment is the literal "_".
const (
	PathPrefixValidate = "/admission/validate/"
	PathPrefixMutate   = "/admission/mutate/"

	// clusterNSToken is the path segment used in place of a namespace for
	// cluster-scoped resources. JSAdmission is cluster-scoped today, so every
	// path will use this; we keep it explicit to leave room for namespaced
	// admission policies in the future.
	clusterNSToken = "cluster"
)

// PolicyEntry is what the server needs to know about a single JSAdmission
// policy in order to dispatch a request to it.
// jsadmission.R10
type PolicyEntry struct {
	Key           types.NamespacedName
	Mutating      bool
	Timeout       time.Duration
	FailurePolicy admissionregv1.FailurePolicyType

	// Emit publishes lifecycle events about this policy (panic/timeout/JS
	// error during a review). Nil-safe; Register accepts entries without
	// an emitter and the review path no-ops emission.
	Emit EventEmitter
}

// Server is the HTTP-level admission dispatcher. It holds the live policy
// table and calls the per-policy script through the Runner on every request.
//
// Mount the handlers on controller-runtime's WebhookServer:
//
//	wh.Register(admission.PathPrefixValidate, srv.ValidateHandler())
//	wh.Register(admission.PathPrefixMutate,   srv.MutateHandler())
type Server struct {
	Runner jsrun.Runner
	Log    logr.Logger

	mu       sync.RWMutex
	policies map[types.NamespacedName]PolicyEntry

	codecs serializer.CodecFactory
}

// NewServer returns a Server backed by the given runner. Pass the same
// Runner the JSAdmissionReconciler uses to load policy code.
func NewServer(rt jsrun.Runner, log logr.Logger) *Server {
	scheme := runtime.NewScheme()
	_ = admissionv1.AddToScheme(scheme)
	return &Server{
		Runner:   rt,
		Log:      log,
		policies: make(map[types.NamespacedName]PolicyEntry),
		codecs:   serializer.NewCodecFactory(scheme),
	}
}

// Register publishes a policy. Idempotent — replacing an entry just updates
// timeout/failurePolicy/mutating in place.
func (s *Server) Register(entry PolicyEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[entry.Key] = entry
}

// Unregister drops the policy. After this returns, requests for that path
// answer 404. Idempotent.
func (s *Server) Unregister(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.policies, key)
}

func (s *Server) lookup(key types.NamespacedName) (PolicyEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.policies[key]
	return e, ok
}

// PathFor returns the URL path the apiserver should send requests to for a
// given policy. Mirrors the parsing in keyFromPath.
func PathFor(key types.NamespacedName, mutating bool) string {
	prefix := PathPrefixValidate
	if mutating {
		prefix = PathPrefixMutate
	}
	ns := key.Namespace
	if ns == "" {
		ns = clusterNSToken
	}
	return prefix + ns + "/" + key.Name
}

// keyFromPath inverts PathFor on the request URL's path.
func keyFromPath(path, prefix string) (types.NamespacedName, bool) {
	tail := strings.TrimPrefix(path, prefix)
	if tail == path { // not the expected prefix
		return types.NamespacedName{}, false
	}
	parts := strings.SplitN(tail, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return types.NamespacedName{}, false
	}
	ns := parts[0]
	if ns == clusterNSToken {
		ns = ""
	}
	return types.NamespacedName{Namespace: ns, Name: parts[1]}, true
}

// ValidateHandler is the HTTP handler for /admission/validate/.
func (s *Server) ValidateHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, PathPrefixValidate, false)
	})
}

// MutateHandler is the HTTP handler for /admission/mutate/.
func (s *Server) MutateHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, PathPrefixMutate, true)
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, prefix string, mutating bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key, ok := keyFromPath(r.URL.Path, prefix)
	if !ok {
		http.Error(w, "bad admission path", http.StatusNotFound)
		return
	}
	entry, known := s.lookup(key)
	if !known || entry.Mutating != mutating {
		http.Error(w, fmt.Sprintf("unknown admission policy %s", key), http.StatusNotFound)
		return
	}

	body, err := readBody(r, maxBodyBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	review, err := s.decodeReview(body)
	if err != nil {
		http.Error(w, fmt.Sprintf("decode AdmissionReview: %v", err), http.StatusBadRequest)
		return
	}
	if review.Request == nil {
		http.Error(w, "AdmissionReview.request is nil", http.StatusBadRequest)
		return
	}

	resp := s.review(r, entry, review.Request)

	out := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionv1.SchemeGroupVersion.String(),
			Kind:       "AdmissionReview",
		},
		Response: resp,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.Log.Error(err, "encode AdmissionReview response", "policy", key)
	}
}

// publishEntry forwards a Warning/Normal event to the emitter on entry, if
// any. Static, low-cardinality messages only — see plan/Message stability.
func publishEntry(entry PolicyEntry, eventType, reason, message string) {
	if entry.Emit != nil {
		entry.Emit(eventType, reason, message)
	}
}

// review runs the policy and produces an AdmissionResponse. UID is always
// echoed from the request. failurePolicy decides Allowed on JS errors.
//
// Uses the Runner, which serializes calls, applies the per-call deadline and
// classifies the outcome (panic / OOM / cancelled / error / ok) under one
// roof. The previous goroutine + time.NewTimer dance is gone because wazero now actually cancels the
// in-flight wasm call when the deadline fires (CloseOnContextDone), so we
// can rescue the VM on timeout — which the old "leave the goroutine
// running" approach couldn't do safely.
// jsadmission.R12
// jsadmission.R14
func (s *Server) review(r *http.Request, entry PolicyEntry, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	log := s.Log.WithValues("policy", entry.Key, "uid", req.UID)
	resp := &admissionv1.AdmissionResponse{UID: req.UID}

	if _, ok := s.Runner.Instance(jsrun.AdmissionKey(entry.Key)); !ok {
		log.Info("admission instance not loaded yet — applying failurePolicy")
		applyFailurePolicy(resp, entry.FailurePolicy, "policy instance not loaded")
		return resp
	}

	jsReq, err := buildJSRequest(req)
	if err != nil {
		log.Error(err, "build JS request")
		applyFailurePolicy(resp, entry.FailurePolicy, fmt.Sprintf("build request: %v", err))
		return resp
	}

	timeout := entry.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	callCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	result, res, callErr := Handle(callCtx, s.Runner, jsrun.AdmissionKey(entry.Key), jsReq, entry.Mutating)
	if callErr != nil {
		// ErrUnknownKey: VM was dropped between Get and Call (race with controller delete).
		log.Info("admission instance vanished mid-review — applying failurePolicy")
		applyFailurePolicy(resp, entry.FailurePolicy, "policy instance not loaded")
		return resp
	}

	switch res.Outcome {
	case jsrun.OutcomePanic:
		log.Error(fmt.Errorf("panic in admission handler: %v", res.Panic),
			"admission JS call panicked — rescuing")
		publishEntry(entry, corev1.EventTypeWarning,
			conditions.EventReviewPanicked,
			"panic in admission handler")
		if err := jslifecycle.Rescue(s.Runner, jsrun.AdmissionKey(entry.Key), jsrun.ReasonPanic, entry.Emit); err != nil {
			log.Error(err, "admission rescue failed", "reason", jsrun.ReasonPanic)
		}
		applyFailurePolicy(resp, entry.FailurePolicy, fmt.Sprintf("panic in admission handler: %v", res.Panic))
		return resp

	case jsrun.OutcomeMemoryLimit:
		log.Error(res.Err, "admission JS call hit memory limit — rescuing")
		if err := jslifecycle.Rescue(s.Runner, jsrun.AdmissionKey(entry.Key), jsrun.ReasonMemoryLimit, entry.Emit); err != nil {
			log.Error(err, "admission rescue failed", "reason", jsrun.ReasonMemoryLimit)
		}
		applyFailurePolicy(resp, entry.FailurePolicy, res.Err.Error())
		return resp

	case jsrun.OutcomeCancelled:
		// Distinguish the two cancellation sources: client disconnect vs
		// our deadline. Client disconnect is benign and the VM is fine —
		// don't rescue. Our deadline tripped → VM is genuinely stuck on a
		// bad call; rescue it so the next request gets a fresh runtime.
		if r.Context().Err() != nil && callCtx.Err() != nil && r.Context().Err() == context.Canceled {
			log.Info("client disconnected during admission review")
			applyFailurePolicy(resp, entry.FailurePolicy, "client disconnected")
			return resp
		}
		log.Info("admission JS call exceeded timeout — rescuing", "timeout", timeout)
		publishEntry(entry, corev1.EventTypeWarning,
			conditions.EventReviewTimeout,
			fmt.Sprintf("review exceeded %s", timeout))
		if err := jslifecycle.Rescue(s.Runner, jsrun.AdmissionKey(entry.Key), jsrun.ReasonTimeout, entry.Emit); err != nil {
			log.Error(err, "admission rescue failed", "reason", jsrun.ReasonTimeout)
		}
		applyFailurePolicy(resp, entry.FailurePolicy, fmt.Sprintf("timeout after %s", timeout))
		return resp

	case jsrun.OutcomeError:
		// Regular JS Error from validate()/mutate(): the policy author
		// returned/threw. Don't rescue — the VM is still healthy.
		log.Error(res.Err, "admission JS call failed")
		publishEntry(entry, corev1.EventTypeWarning,
			conditions.EventReviewFailed,
			"review returned an error")
		applyFailurePolicy(resp, entry.FailurePolicy, res.Err.Error())
		return resp

	default: // OutcomeOK
		fillResponse(resp, result, entry, req, log)
		return resp
	}
}

// fillResponse maps a runtime.AdmissionResult onto an AdmissionResponse,
// computing the JSONPatch for mutating policies.
// jsadmission.R5
// jsadmission.R6
// jsadmission.R7
// jsadmission.R8
// jsadmission.R16
func fillResponse(resp *admissionv1.AdmissionResponse, result *AdmissionResult, entry PolicyEntry, req *admissionv1.AdmissionRequest, log logr.Logger) {
	resp.Allowed = result.Allowed
	if result.Message != "" || result.Code != 0 {
		resp.Result = &metav1.Status{Message: result.Message, Code: result.Code}
	}
	if len(result.Warnings) > 0 {
		resp.Warnings = result.Warnings
	}
	if !entry.Mutating {
		if result.ModifiedObject != nil {
			log.Info("validating policy returned modifiedObject — ignored")
		}
		return
	}
	if !result.Allowed {
		// jsadmission.R16: a denied request carries no patch.
		return
	}
	if result.ModifiedObject == nil {
		return
	}
	ops, err := CreatePatch(req.Object.Raw, result.ModifiedObject)
	if err != nil {
		log.Error(err, "create JSONPatch from modifiedObject")
		// Don't reject the request just because the patch failed — the policy
		// already said allowed=true. Surface the issue as a warning.
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("gojsop: failed to compute patch: %v", err))
		return
	}
	if len(ops) == 0 {
		return
	}
	patchBytes, err := json.Marshal(ops)
	if err != nil {
		log.Error(err, "marshal JSONPatch")
		return
	}
	resp.Patch = patchBytes
	pt := admissionv1.PatchTypeJSONPatch
	resp.PatchType = &pt
}

// applyFailurePolicy sets resp.Allowed and a Status.Message based on the
// configured failurePolicy. Empty/Fail → denied; Ignore → allowed (with the
// reason logged out of band by the caller).
// jsadmission.R9
func applyFailurePolicy(resp *admissionv1.AdmissionResponse, fp admissionregv1.FailurePolicyType, reason string) {
	if fp == admissionregv1.Ignore {
		resp.Allowed = true
		resp.Warnings = append(resp.Warnings, "gojsop: "+reason)
		return
	}
	resp.Allowed = false
	resp.Result = &metav1.Status{Message: "gojsop admission error: " + reason, Code: http.StatusInternalServerError}
}

// buildJSRequest converts an apiserver AdmissionRequest into the typed
// AdmissionRequest that JS sees as `req`. Object/oldObject are unmarshaled
// here so JS gets real objects, not RawExtension byte slices.
func buildJSRequest(req *admissionv1.AdmissionRequest) (*AdmissionRequest, error) {
	extra := make(map[string][]string, len(req.UserInfo.Extra))
	for k, v := range req.UserInfo.Extra {
		extra[k] = []string(v)
	}
	out := &AdmissionRequest{
		UID:         string(req.UID),
		Kind:        GroupVersionKind{Group: req.Kind.Group, Version: req.Kind.Version, Kind: req.Kind.Kind},
		Resource:    GroupVersionResource{Group: req.Resource.Group, Version: req.Resource.Version, Resource: req.Resource.Resource},
		SubResource: req.SubResource,
		Name:        req.Name,
		Namespace:   req.Namespace,
		Operation:   string(req.Operation),
		UserInfo: UserInfo{
			Username: req.UserInfo.Username,
			UID:      req.UserInfo.UID,
			Groups:   req.UserInfo.Groups,
			Extra:    extra,
		},
		DryRun: req.DryRun != nil && *req.DryRun,
	}
	if len(req.Object.Raw) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(req.Object.Raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal request.object: %w", err)
		}
		out.Object = obj
	}
	if len(req.OldObject.Raw) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(req.OldObject.Raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal request.oldObject: %w", err)
		}
		out.OldObject = obj
	}
	return out, nil
}

// maxBodyBytes caps the AdmissionReview payload at 3 MiB. The apiserver
// itself sends well under this; the cap is a defense against accidents.
const maxBodyBytes = 3 << 20

func readBody(r *http.Request, max int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, max)
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

// decodeReview parses the AdmissionReview from the request body. The
// apiserver sends both v1 and v1beta1 historically; we accept the v1 wire
// form only — that's all admissionRegistration v1 negotiates.
func (s *Server) decodeReview(body []byte) (*admissionv1.AdmissionReview, error) {
	var review admissionv1.AdmissionReview
	gvk := schema.GroupVersionKind{Group: admissionv1.GroupName, Version: "v1", Kind: "AdmissionReview"}
	_, _, err := s.codecs.UniversalDeserializer().Decode(body, &gvk, &review)
	if err != nil {
		// Fall back to plain JSON decode — some test clients don't set
		// apiVersion/kind on the wire.
		if jerr := json.Unmarshal(body, &review); jerr != nil {
			return nil, fmt.Errorf("%w (json fallback: %v)", err, jerr)
		}
	}
	return &review, nil
}
