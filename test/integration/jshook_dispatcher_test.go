package integration

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
	"github.com/efuturetoday/gojsop/internal/jsengine"
	"github.com/efuturetoday/gojsop/internal/jsengine/kubehost"
	"github.com/efuturetoday/gojsop/internal/jshook/dispatcher"
	"github.com/efuturetoday/gojsop/internal/jsregistry"
	"github.com/efuturetoday/gojsop/internal/jsregistry/registrytest"
	"github.com/efuturetoday/gojsop/internal/jsrun"
)

type configMapMapper struct{}

func (configMapMapper) KindFor(schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, nil
}

var _ = Describe("JSHook dispatcher against a real API server", func() {
	// Every call starts from the snapshot, so the events are logged on the
	// Go side through the host function record().
	const src = `
function handle(e) { record(e); }`

	It("delivers only objects of the selected namespace and labels", func() {
		// jshook.R15
		ctx := context.Background()
		key := types.NamespacedName{Name: "selector-hook"}

		mkNS := func(name string) {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
		}
		mkCM := func(ns, name string, lbl map[string]string) {
			Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl},
			})).To(Succeed())
		}
		mkNS("sel-a")
		mkNS("sel-b")
		match := map[string]string{"app": "x"}
		mkCM("sel-a", "match-1", match)
		mkCM("sel-a", "no-label", nil)
		mkCM("sel-a", "other-label", map[string]string{"app": "y"})
		mkCM("sel-b", "other-ns", match)

		reg := jsregistry.NewRegistry()
		var (
			mu  sync.Mutex
			log []json.RawMessage
		)
		record := jsengine.HostBinderFunc(func(h *jsengine.Host) error {
			h.Func("record", func(_ context.Context, arg json.RawMessage) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				log = append(log, append(json.RawMessage(nil), arg...))
				return nil, nil
			})
			return nil
		})
		host := jsengine.Binders(record, kubehost.HookEvents{})
		opts := jsrun.Spec{Source: []byte(src), SourceHash: "s", Limits: jsengine.Limits{}, Host: host}
		_, _, err := registrytest.GetOrLoad(reg, ctx, jsrun.HookKey(key), opts)
		Expect(err).NotTo(HaveOccurred())
		dyn, err := dynamic.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		d := dispatcher.New(dyn, configMapMapper{}, reg)
		DeferCleanup(func() {
			d.Drop(key)
			reg.Drop(jsrun.HookKey(key))
		})

		// A namespace is selected by the label the apiserver sets on every
		// namespace since 1.21 — there is no separate name field (jshook.R22).
		binding := corev1alpha1.HookBinding{
			Name: "cms",
			ResourceRule: corev1alpha1.ResourceRule{
				APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"},
			},
			ObjectMatch: corev1alpha1.ObjectMatch{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{corev1.LabelMetadataName: "sel-a"},
				},
				ObjectSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
			},
		}
		Expect(d.Subscribe(ctx, key, []corev1alpha1.HookBinding{binding}, nil, nil)).To(Succeed())

		// Created after the watch started: one match, two misses, one match.
		mkCM("sel-a", "match-2", match)
		mkCM("sel-a", "no-label-2", nil)
		mkCM("sel-b", "other-ns-2", match)
		mkCM("sel-a", "match-3", match)

		type meta struct{ Metadata struct{ Name string } }
		delivered := func() []string {
			mu.Lock()
			raw := append([]json.RawMessage(nil), log...)
			mu.Unlock()
			var names []string
			for _, r := range raw {
				var ev struct{ Object meta }
				Expect(json.Unmarshal(r, &ev)).To(Succeed())
				names = append(names, ev.Object.Metadata.Name)
			}
			return names
		}
		Eventually(delivered, 20*time.Second, 100*time.Millisecond).Should(ConsistOf("match-1", "match-2", "match-3"))
		Consistently(delivered, 2*time.Second, 200*time.Millisecond).Should(ConsistOf("match-1", "match-2", "match-3"))
	})
})
