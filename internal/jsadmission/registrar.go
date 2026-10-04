package jsadmission

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Names of the central WebhookConfiguration objects gojsop owns.
const (
	ValidatingConfigName = "gojsop-validating"
	MutatingConfigName   = "gojsop-mutating"
)

// CABundleProvider returns the PEM-encoded CA bundle the apiserver should
// trust when calling our webhook. Called on every Sync and every CAResync
// tick, so a cert-manager renewal reaches the configurations on its own.
type CABundleProvider func(ctx context.Context) ([]byte, error)

// PolicyMeta is everything the registrar needs to write a single
// webhooks[] entry into the central VWC/MWC.
// jsadmission.R10
type PolicyMeta struct {
	Key            types.NamespacedName
	Path           string
	Mutating       bool
	Rules          []admissionregv1.RuleWithOperations
	FailurePolicy  admissionregv1.FailurePolicyType
	MatchPolicy    admissionregv1.MatchPolicyType
	SideEffects    admissionregv1.SideEffectClass
	TimeoutSeconds int32
	NSSelector     *metav1.LabelSelector
	ObjectSelector *metav1.LabelSelector
	// ReinvocationPolicy applies to mutating webhooks only. Empty → apiserver default.
	ReinvocationPolicy *admissionregv1.ReinvocationPolicyType
}

// Registrar maintains the two central *WebhookConfigurations
// (`gojsop-validating`, `gojsop-mutating`). Each JSAdmission becomes one
// entry in the matching `.webhooks[]`. Sync writes the current state atomically.
//
// Calls to Upsert/Remove are debounced — Sync runs at most once per debounce
// window so a controller startup that reconciles N policies issues just one
// VWC/MWC write.
type Registrar struct {
	Client     client.Client
	Service    admissionregv1.ServiceReference // {namespace, name, port=443}
	CAProvider CABundleProvider
	Log        logr.Logger
	Debounce   time.Duration
	// RetryDelay is how long a failed background Sync waits before it is
	// tried again. Zero means 5 s.
	RetryDelay time.Duration
	// OnSyncResult, when set, is called after a background Sync whenever the
	// outcome changes between success and failure (err is nil on success).
	// The controller uses it to re-reconcile policies so the failure shows
	// in their status. It must not block.
	OnSyncResult func(err error)
	// ExcludeNamespaces is merged into every webhook's namespaceSelector via a
	// NotIn matchExpression on `kubernetes.io/metadata.name`. The controller's
	// own namespace plus kube-system / cert-manager belong here so a broken
	// policy can't lock the controller out of its own pod re-creation.
	ExcludeNamespaces []string
	// CAResync is how often the registrar reads the CA bundle and writes both
	// configurations again when it changed since the last Sync. Zero means
	// one minute.
	CAResync time.Duration

	mu       sync.Mutex
	policies map[types.NamespacedName]PolicyMeta
	dirty    bool
	timer    *time.Timer
	syncCtx  context.Context
	syncErr  error
	// lastCA is the CA bundle the last successful Sync wrote.
	lastCA []byte
}

// NewRegistrar wires a Registrar to its dependencies. Pass the same
// controller-runtime client the reconciler uses.
func NewRegistrar(c client.Client, svc admissionregv1.ServiceReference, ca CABundleProvider, log logr.Logger) *Registrar {
	return &Registrar{
		Client:     c,
		Service:    svc,
		CAProvider: ca,
		Log:        log,
		Debounce:   200 * time.Millisecond,
		policies:   make(map[types.NamespacedName]PolicyMeta),
	}
}

// Start binds a context to background Syncs. Until called, Upsert/Remove
// only update the in-memory map — Sync is a no-op without a context.
// SetupWithManager is expected to call Start with the manager's context.
func (r *Registrar) Start(ctx context.Context) {
	r.mu.Lock()
	r.syncCtx = ctx
	r.mu.Unlock()
	go r.watchCA(ctx)
}

// watchCA writes both configurations again when the CA bundle changed since
// the last successful Sync. cert-manager renews the serving certificate on
// its own; without this the apiserver keeps the old CA until a policy
// changes, and every admission call fails TLS until then.
// jsadmission.R26
func (r *Registrar) watchCA(ctx context.Context) {
	d := r.CAResync
	if d <= 0 {
		d = time.Minute
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ca, err := r.CAProvider(ctx)
		if err != nil {
			r.Log.Error(err, "reading CA bundle")
			continue
		}
		r.mu.Lock()
		// A pending Sync reads the CA anyway; scheduling again would only
		// push it back.
		if !r.dirty && r.lastCA != nil && !bytes.Equal(ca, r.lastCA) {
			r.Log.Info("CA bundle changed, rewriting webhook configurations")
			r.scheduleSyncLocked()
		}
		r.mu.Unlock()
	}
}

// Upsert publishes a policy. Triggers a debounced sync.
func (r *Registrar) Upsert(meta PolicyMeta) {
	r.mu.Lock()
	r.policies[meta.Key] = meta
	r.scheduleSyncLocked()
	r.mu.Unlock()
}

// Remove drops a policy. Triggers a debounced sync.
func (r *Registrar) Remove(key types.NamespacedName) {
	r.mu.Lock()
	if _, ok := r.policies[key]; ok {
		delete(r.policies, key)
		r.scheduleSyncLocked()
	}
	r.mu.Unlock()
}

func (r *Registrar) scheduleSyncLocked() {
	r.dirty = true
	if r.syncCtx == nil {
		return
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	d := r.Debounce
	if d <= 0 {
		d = 200 * time.Millisecond
	}
	r.armTimerLocked(r.syncCtx, d)
}

// armTimerLocked runs a background Sync after d. A failed Sync is recorded,
// reported through OnSyncResult and retried after RetryDelay until it
// succeeds or a newer Upsert/Remove takes over.
func (r *Registrar) armTimerLocked(ctx context.Context, d time.Duration) {
	r.timer = time.AfterFunc(d, func() {
		err := r.Sync(ctx)
		if err != nil {
			r.Log.Error(err, "registrar Sync failed")
		}
		r.mu.Lock()
		changed := (err == nil) != (r.syncErr == nil)
		r.syncErr = err
		notify := r.OnSyncResult
		if err != nil && ctx.Err() == nil {
			retry := r.RetryDelay
			if retry <= 0 {
				retry = 5 * time.Second
			}
			r.armTimerLocked(ctx, retry)
		}
		r.mu.Unlock()
		if changed && notify != nil {
			notify(err)
		}
	})
}

// SyncError returns the error of the last background Sync, nil when it
// succeeded or none ran yet.
func (r *Registrar) SyncError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.syncErr
}

// Keys lists the policies currently published.
func (r *Registrar) Keys() []types.NamespacedName {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]types.NamespacedName, 0, len(r.policies))
	for k := range r.policies {
		keys = append(keys, k)
	}
	return keys
}

// Sync rewrites both central WebhookConfigurations from the live policy map.
// Side-effect: drops a config object when it has no entries left.
func (r *Registrar) Sync(ctx context.Context) error {
	r.mu.Lock()
	policies := make([]PolicyMeta, 0, len(r.policies))
	for _, p := range r.policies {
		policies = append(policies, p)
	}
	r.dirty = false
	r.mu.Unlock()

	sort.Slice(policies, func(i, j int) bool {
		if policies[i].Key.Namespace != policies[j].Key.Namespace {
			return policies[i].Key.Namespace < policies[j].Key.Namespace
		}
		return policies[i].Key.Name < policies[j].Key.Name
	})

	caBundle, err := r.CAProvider(ctx)
	if err != nil {
		return fmt.Errorf("ca provider: %w", err)
	}

	var validating, mutating []PolicyMeta
	for _, p := range policies {
		if p.Mutating {
			mutating = append(mutating, p)
		} else {
			validating = append(validating, p)
		}
	}
	if err := r.applyValidating(ctx, validating, caBundle); err != nil {
		return err
	}
	if err := r.applyMutating(ctx, mutating, caBundle); err != nil {
		return err
	}
	r.mu.Lock()
	r.lastCA = caBundle
	r.mu.Unlock()
	return nil
}

func (r *Registrar) applyValidating(ctx context.Context, policies []PolicyMeta, caBundle []byte) error {
	if len(policies) == 0 {
		return r.deleteIfExists(ctx, &admissionregv1.ValidatingWebhookConfiguration{}, ValidatingConfigName)
	}
	desired := &admissionregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: ValidatingConfigName},
	}
	for _, p := range policies {
		desired.Webhooks = append(desired.Webhooks, admissionregv1.ValidatingWebhook{
			Name:                    DNSWebhookName(p.Key),
			ClientConfig:            r.clientConfig(p.Path, caBundle),
			Rules:                   p.Rules,
			FailurePolicy:           ptr(orFailDefault(p.FailurePolicy)),
			MatchPolicy:             ptr(orMatchDefault(p.MatchPolicy)),
			SideEffects:             ptr(orSideEffectDefault(p.SideEffects)),
			TimeoutSeconds:          ptr(orTimeoutDefault(p.TimeoutSeconds)),
			AdmissionReviewVersions: []string{"v1"},
			NamespaceSelector:       r.mergeNSSelector(p.NSSelector),
			ObjectSelector:          p.ObjectSelector,
		})
	}
	return r.upsert(ctx, desired, &admissionregv1.ValidatingWebhookConfiguration{})
}

func (r *Registrar) applyMutating(ctx context.Context, policies []PolicyMeta, caBundle []byte) error {
	if len(policies) == 0 {
		return r.deleteIfExists(ctx, &admissionregv1.MutatingWebhookConfiguration{}, MutatingConfigName)
	}
	desired := &admissionregv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: MutatingConfigName},
	}
	for _, p := range policies {
		desired.Webhooks = append(desired.Webhooks, admissionregv1.MutatingWebhook{
			Name:                    DNSWebhookName(p.Key),
			ClientConfig:            r.clientConfig(p.Path, caBundle),
			Rules:                   p.Rules,
			FailurePolicy:           ptr(orFailDefault(p.FailurePolicy)),
			MatchPolicy:             ptr(orMatchDefault(p.MatchPolicy)),
			SideEffects:             ptr(orSideEffectDefault(p.SideEffects)),
			TimeoutSeconds:          ptr(orTimeoutDefault(p.TimeoutSeconds)),
			AdmissionReviewVersions: []string{"v1"},
			NamespaceSelector:       r.mergeNSSelector(p.NSSelector),
			ObjectSelector:          p.ObjectSelector,
			ReinvocationPolicy:      p.ReinvocationPolicy,
		})
	}
	return r.upsert(ctx, desired, &admissionregv1.MutatingWebhookConfiguration{})
}

func (r *Registrar) clientConfig(path string, caBundle []byte) admissionregv1.WebhookClientConfig {
	svc := r.Service
	svc.Path = ptr(path)
	return admissionregv1.WebhookClientConfig{Service: &svc, CABundle: caBundle}
}

// mergeNSSelector folds Registrar.ExcludeNamespaces into the user's selector
// via a NotIn matchExpression on the well-known `kubernetes.io/metadata.name`
// label, so the controller's own namespace can never be matched.
func (r *Registrar) mergeNSSelector(user *metav1.LabelSelector) *metav1.LabelSelector {
	if len(r.ExcludeNamespaces) == 0 {
		return user
	}
	out := &metav1.LabelSelector{}
	if user != nil {
		out.MatchLabels = user.MatchLabels
		out.MatchExpressions = append(out.MatchExpressions, user.MatchExpressions...)
	}
	out.MatchExpressions = append(out.MatchExpressions, metav1.LabelSelectorRequirement{
		Key:      "kubernetes.io/metadata.name",
		Operator: metav1.LabelSelectorOpNotIn,
		Values:   append([]string(nil), r.ExcludeNamespaces...),
	})
	return out
}

// upsert applies `desired` via Get+Update / Create. controller-runtime's
// CreateOrUpdate would also work but rolls its own diff — we just clobber
// the spec, since the registrar fully owns these two objects.
func (r *Registrar) upsert(ctx context.Context, desired client.Object, current client.Object) error {
	key := client.ObjectKeyFromObject(desired)
	err := r.Client.Get(ctx, key, current)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	switch d := desired.(type) {
	case *admissionregv1.ValidatingWebhookConfiguration:
		c := current.(*admissionregv1.ValidatingWebhookConfiguration)
		c.Webhooks = d.Webhooks
		return r.Client.Update(ctx, c)
	case *admissionregv1.MutatingWebhookConfiguration:
		c := current.(*admissionregv1.MutatingWebhookConfiguration)
		c.Webhooks = d.Webhooks
		return r.Client.Update(ctx, c)
	}
	return fmt.Errorf("registrar: unsupported object type %T", desired)
}

func (r *Registrar) deleteIfExists(ctx context.Context, obj client.Object, name string) error {
	obj.SetName(name)
	if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// DNSWebhookName produces the DNS-1123-conformant name used inside
// webhooks[].name. The apiserver rejects names that don't match this shape.
func DNSWebhookName(key types.NamespacedName) string {
	if key.Namespace == "" {
		return fmt.Sprintf("%s.policies.gojsop.io", key.Name)
	}
	return fmt.Sprintf("%s-%s.policies.gojsop.io", key.Namespace, key.Name)
}

// RulesFromAPI converts the CRD shape (v1alpha1.AdmissionRule) into the
// admissionregistration.k8s.io/v1 shape. Lives in this package so the API
// types stay agnostic of the apiserver wire types.
func RulesFromAPI(rules []APIRule) []admissionregv1.RuleWithOperations {
	out := make([]admissionregv1.RuleWithOperations, 0, len(rules))
	for _, r := range rules {
		ops := make([]admissionregv1.OperationType, 0, len(r.Operations))
		for _, op := range r.Operations {
			ops = append(ops, admissionregv1.OperationType(op))
		}
		var scope *admissionregv1.ScopeType
		if r.Scope != "" {
			s := admissionregv1.ScopeType(r.Scope)
			scope = &s
		}
		out = append(out, admissionregv1.RuleWithOperations{
			Operations: ops,
			Rule: admissionregv1.Rule{
				APIGroups:   r.APIGroups,
				APIVersions: r.APIVersions,
				Resources:   r.Resources,
				Scope:       scope,
			},
		})
	}
	return out
}

// APIRule mirrors the JSAdmission CRD's AdmissionRule shape. We accept it as
// a plain struct here so the admission package doesn't depend on the api
// package — keeps the dependency graph one-way (controller → admission).
type APIRule struct {
	APIGroups   []string
	APIVersions []string
	Resources   []string
	Operations  []string
	Scope       string
}

// ----- Defaults helpers -----

func orFailDefault(v admissionregv1.FailurePolicyType) admissionregv1.FailurePolicyType {
	if v == "" {
		return admissionregv1.Fail
	}
	return v
}

func orMatchDefault(v admissionregv1.MatchPolicyType) admissionregv1.MatchPolicyType {
	if v == "" {
		return admissionregv1.Equivalent
	}
	return v
}

func orSideEffectDefault(v admissionregv1.SideEffectClass) admissionregv1.SideEffectClass {
	if v == "" {
		return admissionregv1.SideEffectClassNone
	}
	return v
}

func orTimeoutDefault(v int32) int32 {
	if v <= 0 {
		return 5
	}
	return v
}

func ptr[T any](v T) *T { return &v }
