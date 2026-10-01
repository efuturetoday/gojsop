package integration

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jshook"
	"github.com/o-haase/gojsop/internal/jshook/dispatcher"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsregistry/registrytest"
	"github.com/o-haase/gojsop/internal/jsrun"
)

type configMapMapper struct{}

func (configMapMapper) RESTMapping(schema.GroupKind, ...string) (*dispatcher.RESTMapping, error) {
	return &dispatcher.RESTMapping{Resource: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}}, nil
}

var _ = Describe("JSHook dispatcher against a real API server", func() {
	const src = `
globalThis.log = [];
function handle(c) { log.push(c[0]); }`

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
		opts := jsrun.Spec{Source: []byte(src), SourceHash: "s", Limits: jsengine.Limits{}}
		_, _, err := registrytest.GetOrLoad(reg, ctx, jsrun.HookKey(key), opts)
		Expect(err).NotTo(HaveOccurred())
		dyn, err := dynamic.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		d := dispatcher.New(dyn, configMapMapper{}, reg)
		DeferCleanup(func() {
			d.Drop(key)
			reg.Drop(jsrun.HookKey(key))
		})

		binding := jshook.KubernetesBinding{
			Name: "cms", APIVersion: "v1", Kind: "ConfigMap",
			Namespace:     &jshook.NamespaceSel{NameSelector: &jshook.NameSelector{MatchNames: []string{"sel-a"}}},
			LabelSelector: map[string]any{"matchLabels": map[string]any{"app": "x"}},
		}
		Expect(d.Subscribe(ctx, key, &jshook.Config{Kubernetes: []jshook.KubernetesBinding{binding}}, nil)).To(Succeed())

		// Created after the snapshot: one match, two misses, one match.
		mkCM("sel-a", "match-2", match)
		mkCM("sel-a", "no-label-2", nil)
		mkCM("sel-b", "other-ns-2", match)
		mkCM("sel-a", "match-3", match)

		type meta struct{ Metadata struct{ Name string } }
		delivered := func() []string {
			var out string
			_, _, err := reg.Call(ctx, jsrun.HookKey(key), func(c context.Context, vm *jsengine.VM) error {
				var err error
				out, err = vm.Eval(c, "log.js", `JSON.stringify(globalThis.log)`)
				return err
			})
			Expect(err).NotTo(HaveOccurred())
			var cs []struct {
				Type    string
				Object  meta
				Objects []struct{ Object meta }
			}
			Expect(json.Unmarshal([]byte(out), &cs)).To(Succeed())
			var names []string
			for _, c := range cs {
				if c.Type == "Synchronization" {
					for _, o := range c.Objects {
						names = append(names, o.Object.Metadata.Name)
					}
					continue
				}
				names = append(names, c.Object.Metadata.Name)
			}
			return names
		}
		Eventually(delivered, 20*time.Second, 100*time.Millisecond).Should(ConsistOf("match-1", "match-2", "match-3"))
		Consistently(delivered, 2*time.Second, 200*time.Millisecond).Should(ConsistOf("match-1", "match-2", "match-3"))
	})
})
