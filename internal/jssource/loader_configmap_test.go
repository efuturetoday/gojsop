package jssource

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
)

func newReader(t *testing.T, objs ...runtime.Object) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add gojsop scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

// js-sources.R8
func TestConfigMapLoader_DefaultKey(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "ns1"},
		Data:       map[string]string{"hook.js": "function handle(){}"},
	}
	c := newReader(t, cm).Build()
	l := ConfigMapLoader{Reader: c}
	body, ok, err := l.Load(context.Background(), corev1alpha1.JSSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "policy", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatal("loader claimed no match despite configMapRef set")
	}
	if string(body) != "function handle(){}" {
		t.Fatalf("body: got %q", string(body))
	}
}

// js-sources.R8
func TestConfigMapLoader_ExplicitKey(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "ns1"},
		Data:       map[string]string{"main.js": "// from main.js"},
	}
	c := newReader(t, cm).Build()
	l := ConfigMapLoader{Reader: c}
	body, _, err := l.Load(context.Background(), corev1alpha1.JSSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "policy", Namespace: "ns1", Key: "main.js"},
	})
	if err != nil || string(body) != "// from main.js" {
		t.Fatalf("explicit key: body=%q err=%v", string(body), err)
	}
}

// js-sources.R2
func TestConfigMapLoader_MissingCM(t *testing.T) {
	c := newReader(t).Build()
	l := ConfigMapLoader{Reader: c}
	_, claimed, err := l.Load(context.Background(), corev1alpha1.JSSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "absent", Namespace: "ns1"},
	})
	if err == nil {
		t.Fatal("missing CM: want error")
	}
	if !claimed {
		t.Fatal("missing CM: loader must claim the source so Chain doesn't fall through to a 'no loader matched' message")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing CM: error should be precise, got %v", err)
	}
}

// js-sources.R2
func TestConfigMapLoader_MissingKey(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "ns1"},
		Data:       map[string]string{"other.js": "// other"},
	}
	c := newReader(t, cm).Build()
	l := ConfigMapLoader{Reader: c}
	_, _, err := l.Load(context.Background(), corev1alpha1.JSSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "policy", Namespace: "ns1"},
	})
	if err == nil || !strings.Contains(err.Error(), "no key") {
		t.Fatalf("missing default key: got err=%v", err)
	}
}

// js-sources.R2
func TestConfigMapLoader_EmptyValue(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "ns1"},
		Data:       map[string]string{"hook.js": ""},
	}
	c := newReader(t, cm).Build()
	l := ConfigMapLoader{Reader: c}
	_, _, err := l.Load(context.Background(), corev1alpha1.JSSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "policy", Namespace: "ns1"},
	})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty value: got err=%v", err)
	}
}

// js-sources.R2
func TestConfigMapLoader_NoRefFallsThrough(t *testing.T) {
	c := newReader(t).Build()
	l := ConfigMapLoader{Reader: c}
	_, claimed, err := l.Load(context.Background(), corev1alpha1.JSSource{Inline: "1+1"})
	if err != nil || claimed {
		t.Fatalf("no configMapRef: claimed=%v err=%v (want claimed=false to let Chain fall through)", claimed, err)
	}
}

// js-sources.R8
func TestConfigMapLoader_BinaryData(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "ns1"},
		BinaryData: map[string][]byte{"hook.js": []byte("// binary delivery")},
	}
	c := newReader(t, cm).Build()
	l := ConfigMapLoader{Reader: c}
	body, _, err := l.Load(context.Background(), corev1alpha1.JSSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "policy", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("binaryData: %v", err)
	}
	if string(body) != "// binary delivery" {
		t.Fatalf("binaryData: body=%q", string(body))
	}
}

// js-sources.R1
func TestChain_ConfigMapAfterInline(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "ns1"},
		Data:       map[string]string{"hook.js": "from-cm"},
	}
	c := newReader(t, cm).Build()
	chain := NewChain(InlineLoader{}, ConfigMapLoader{Reader: c})

	body, err := chain.Load(context.Background(), corev1alpha1.JSSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "policy", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("chain configmap: %v", err)
	}
	if string(body) != "from-cm" {
		t.Fatalf("chain configmap: body=%q", string(body))
	}
}
