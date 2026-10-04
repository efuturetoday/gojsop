package jsadmission

import (
	"context"
	"errors"
	"sync"
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

// jsadmission.R1
func TestRegistrar_TwoValidating_OneVWC_TwoEntries(t *testing.T) {
	r, c := newFakeRegistrar(t)
	ctx := context.Background()
	r.Upsert(samplePolicy("a", false))
	r.Upsert(samplePolicy("b", false))
	if err := r.Sync(ctx); err != nil {
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

// jsadmission.R1
func TestRegistrar_MixedValidatingAndMutating(t *testing.T) {
	r, c := newFakeRegistrar(t)
	ctx := context.Background()
	r.Upsert(samplePolicy("v1", false))
	r.Upsert(samplePolicy("m1", true))
	if err := r.Sync(ctx); err != nil {
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

// jsadmission.R1
func TestRegistrar_RemoveLastEntry_DeletesConfig(t *testing.T) {
	r, c := newFakeRegistrar(t)
	ctx := context.Background()
	r.Upsert(samplePolicy("solo", false))
	if err := r.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	r.Remove(types.NamespacedName{Name: "solo"})
	if err := r.Sync(ctx); err != nil {
		t.Fatalf("Sync remove: %v", err)
	}
	var vwc admissionregv1.ValidatingWebhookConfiguration
	if err := c.Get(ctx, client.ObjectKey{Name: ValidatingConfigName}, &vwc); !apierrors.IsNotFound(err) {
		t.Fatalf("VWC must be deleted when empty: err=%v", err)
	}
}

// jsadmission.R1
func TestRegistrar_Update_OverwritesEntry(t *testing.T) {
	r, c := newFakeRegistrar(t)
	ctx := context.Background()
	r.Upsert(samplePolicy("p", false))
	if err := r.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Mutate the rules.
	updated := samplePolicy("p", false)
	updated.Rules[0].Resources = []string{"deployments"}
	r.Upsert(updated)
	if err := r.Sync(ctx); err != nil {
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

// fakeMetaPolicy ensures the test struct survives ObjectMeta requirements
// (type-check helper to keep the test file self-contained).
var _ = metav1.ObjectMeta{}
var _ = time.Second

// jsadmission.R25
func TestRegistrar_SyncFailure_IsRetriedAndReported(t *testing.T) {
	r, c := newFakeRegistrar(t)
	fails := 2
	var mu sync.Mutex
	r.CAProvider = func(context.Context) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return nil, errors.New("ca unreadable")
		}
		return []byte("CA-PEM"), nil
	}
	r.Debounce = time.Millisecond
	r.RetryDelay = time.Millisecond
	results := make(chan error, 8)
	r.OnSyncResult = func(err error) { results <- err }

	ctx := t.Context()
	r.Start(ctx)
	r.Upsert(samplePolicy("a", false))

	select {
	case err := <-results:
		if err == nil {
			t.Fatal("first result must be the failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sync failure was not reported")
	}
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("recovery reported error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed sync was not retried")
	}
	if err := r.SyncError(); err != nil {
		t.Fatalf("SyncError after recovery: %v", err)
	}
	var got admissionregv1.ValidatingWebhookConfiguration
	if err := c.Get(ctx, client.ObjectKey{Name: ValidatingConfigName}, &got); err != nil {
		t.Fatalf("VWC missing after retry: %v", err)
	}
}

// jsadmission.R26
func TestRegistrar_CAChange_RewritesConfigsWithoutPolicyChange(t *testing.T) {
	r, c := newFakeRegistrar(t)
	var mu sync.Mutex
	ca := "CA-1"
	r.CAProvider = func(context.Context) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		return []byte(ca), nil
	}
	r.CAResync = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.Start(ctx)
	r.Upsert(samplePolicy("a", false))
	r.Upsert(samplePolicy("m", true))

	caOf := func() (string, string) {
		var v admissionregv1.ValidatingWebhookConfiguration
		var m admissionregv1.MutatingWebhookConfiguration
		if c.Get(ctx, client.ObjectKey{Name: ValidatingConfigName}, &v) != nil ||
			c.Get(ctx, client.ObjectKey{Name: MutatingConfigName}, &m) != nil ||
			len(v.Webhooks) == 0 || len(m.Webhooks) == 0 {
			return "", ""
		}
		return string(v.Webhooks[0].ClientConfig.CABundle), string(m.Webhooks[0].ClientConfig.CABundle)
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if v, m := caOf(); v == want && m == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		v, m := caOf()
		t.Fatalf("caBundle: validating %q, mutating %q; want %q in both", v, m, want)
	}
	waitFor("CA-1")

	mu.Lock()
	ca = "CA-2"
	mu.Unlock()
	waitFor("CA-2")
}
