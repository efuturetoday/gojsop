package jsadmission

import (
	"context"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logr "sigs.k8s.io/controller-runtime/pkg/log"
)

func newFakeRegistrar(t *testing.T) (*Registrar, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := admissionregv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	port := int32(443)
	r := NewRegistrar(c,
		admissionregv1.ServiceReference{Namespace: "gojsop-system", Name: "gojsop-webhook", Port: &port},
		func(ctx context.Context) ([]byte, error) { return []byte("CA-PEM"), nil },
		logr.Log)
	r.Debounce = 0
	return r, c
}

func samplePolicy(name string, mutating bool) PolicyMeta {
	key := types.NamespacedName{Name: name}
	return PolicyMeta{
		Key:      key,
		Path:     PathFor(key, mutating),
		Mutating: mutating,
		Rules: []admissionregv1.RuleWithOperations{{
			Operations: []admissionregv1.OperationType{admissionregv1.Create},
			Rule: admissionregv1.Rule{
				APIGroups:   []string{""},
				APIVersions: []string{"v1"},
				Resources:   []string{"pods"},
			},
		}},
		FailurePolicy:  admissionregv1.Fail,
		MatchPolicy:    admissionregv1.Equivalent,
		SideEffects:    admissionregv1.SideEffectClassNone,
		TimeoutSeconds: 5,
	}
}

func TestRegistrar_TwoValidating_OneVWC_TwoEntries(t *testing.T) {
	r, c := newFakeRegistrar(t)
	ctx := context.Background()
	r.Upsert(samplePolicy("a", false))
	r.Upsert(samplePolicy("b", false))
	if err := r.SyncNow(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var got admissionregv1.ValidatingWebhookConfiguration
	if err := c.Get(ctx, client.ObjectKey{Name: ValidatingConfigName}, &got); err != nil {
		t.Fatalf("VWC missing: %v", err)
	}
	if len(got.Webhooks) != 2 {
		t.Fatalf("want 2 webhooks, got %d", len(got.Webhooks))
	}
	for _, wh := range got.Webhooks {
		if string(wh.ClientConfig.CABundle) != "CA-PEM" {
			t.Fatalf("caBundle missing on %s: %q", wh.Name, wh.ClientConfig.CABundle)
		}
		if wh.ClientConfig.Service == nil || *wh.ClientConfig.Service.Path == "" {
			t.Fatalf("service path missing on %s", wh.Name)
		}
	}

	var mwc admissionregv1.MutatingWebhookConfiguration
	if err := c.Get(ctx, client.ObjectKey{Name: MutatingConfigName}, &mwc); !apierrors.IsNotFound(err) {
		t.Fatalf("MWC must not exist when no mutating policies: err=%v", err)
	}
}

func TestRegistrar_MixedValidatingAndMutating(t *testing.T) {
	r, c := newFakeRegistrar(t)
	ctx := context.Background()
	r.Upsert(samplePolicy("v1", false))
	r.Upsert(samplePolicy("m1", true))
	if err := r.SyncNow(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	var vwc admissionregv1.ValidatingWebhookConfiguration
	if err := c.Get(ctx, client.ObjectKey{Name: ValidatingConfigName}, &vwc); err != nil {
		t.Fatalf("VWC missing: %v", err)
	}
	if len(vwc.Webhooks) != 1 || vwc.Webhooks[0].Name != DNSWebhookName(types.NamespacedName{Name: "v1"}) {
		t.Fatalf("validating webhooks: %+v", vwc.Webhooks)
	}
	var mwc admissionregv1.MutatingWebhookConfiguration
	if err := c.Get(ctx, client.ObjectKey{Name: MutatingConfigName}, &mwc); err != nil {
		t.Fatalf("MWC missing: %v", err)
	}
	if len(mwc.Webhooks) != 1 || mwc.Webhooks[0].Name != DNSWebhookName(types.NamespacedName{Name: "m1"}) {
		t.Fatalf("mutating webhooks: %+v", mwc.Webhooks)
	}
}

func TestRegistrar_RemoveLastEntry_DeletesConfig(t *testing.T) {
	r, c := newFakeRegistrar(t)
	ctx := context.Background()
	r.Upsert(samplePolicy("solo", false))
	if err := r.SyncNow(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	r.Remove(types.NamespacedName{Name: "solo"})
	if err := r.SyncNow(ctx); err != nil {
		t.Fatalf("Sync remove: %v", err)
	}
	var vwc admissionregv1.ValidatingWebhookConfiguration
	if err := c.Get(ctx, client.ObjectKey{Name: ValidatingConfigName}, &vwc); !apierrors.IsNotFound(err) {
		t.Fatalf("VWC must be deleted when empty: err=%v", err)
	}
}

func TestRegistrar_Update_OverwritesEntry(t *testing.T) {
	r, c := newFakeRegistrar(t)
	ctx := context.Background()
	r.Upsert(samplePolicy("p", false))
	if err := r.SyncNow(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Mutate the rules.
	updated := samplePolicy("p", false)
	updated.Rules[0].Resources = []string{"deployments"}
	r.Upsert(updated)
	if err := r.SyncNow(ctx); err != nil {
		t.Fatalf("Sync update: %v", err)
	}
	var vwc admissionregv1.ValidatingWebhookConfiguration
	if err := c.Get(ctx, client.ObjectKey{Name: ValidatingConfigName}, &vwc); err != nil {
		t.Fatalf("VWC missing: %v", err)
	}
	if got := vwc.Webhooks[0].Rules[0].Resources; len(got) != 1 || got[0] != "deployments" {
		t.Fatalf("rules not updated: %+v", got)
	}
}

func TestDNSWebhookName_Cluster(t *testing.T) {
	if got := DNSWebhookName(types.NamespacedName{Name: "p"}); got != "p.policies.gojsop.io" {
		t.Fatalf("got %q", got)
	}
	if got := DNSWebhookName(types.NamespacedName{Namespace: "ns", Name: "p"}); got != "ns-p.policies.gojsop.io" {
		t.Fatalf("got %q", got)
	}
}

// fakeMetaPolicy ensures the test struct survives ObjectMeta requirements
// (type-check helper to keep the test file self-contained).
var _ = metav1.ObjectMeta{}
var _ = time.Second
