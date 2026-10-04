package jsaccess

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
	"github.com/efuturetoday/gojsop/internal/jsrun"
)

// kube-access.R10
func TestName_PrefixesKindAndCutsLongNames(t *testing.T) {
	if got := Name(jsrun.KindJSHook, "sync"); got != "jshook-sync" {
		t.Fatalf("hook: got %q", got)
	}
	if got := Name(jsrun.KindJSAdmission, "sync"); got != "jsadmission-sync" {
		t.Fatalf("policy: got %q", got)
	}
	long := strings.Repeat("a", 253)
	a, b := Name(jsrun.KindJSHook, long), Name(jsrun.KindJSHook, long[:252]+"b")
	if len(a) > 253 || len(b) > 253 {
		t.Fatalf("names longer than 253: %d, %d", len(a), len(b))
	}
	if a == b {
		t.Fatal("two long names that differ only at the end must not collide")
	}
}

// kube-access.R11
func TestHookRules_AddReadOnEveryWatchedResource(t *testing.T) {
	spec := corev1alpha1.JSHookSpec{
		Bindings: []corev1alpha1.HookBinding{{
			Name:         "pods",
			ResourceRule: corev1alpha1.ResourceRule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
		}},
		Permissions: []corev1alpha1.Permission{{
			APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []corev1alpha1.PermissionVerb{"get", "create"},
		}},
	}
	got := HookRules(spec)
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "create"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rules, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if strings.Join(got[i].APIGroups, ",") != strings.Join(want[i].APIGroups, ",") ||
			strings.Join(got[i].Resources, ",") != strings.Join(want[i].Resources, ",") ||
			strings.Join(got[i].Verbs, ",") != strings.Join(want[i].Verbs, ",") {
			t.Fatalf("rule %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, rbacv1.AddToScheme, corev1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// kube-access.R10
func TestEnsure_CreatesServiceAccountRoleAndBindingOwnedByTheResource(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	m := &Manager{Client: c, Scheme: scheme, Namespace: "gojsop-system"}
	hook := &corev1alpha1.JSHook{ObjectMeta: metav1.ObjectMeta{Name: "sync", UID: "uid-1"}}
	ctx := context.Background()

	rules := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}}
	sa, err := m.Ensure(ctx, hook, jsrun.KindJSHook, rules)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if sa != "jshook-sync" {
		t.Fatalf("service account %q", sa)
	}

	ownedByHook := func(obj client.Object) {
		t.Helper()
		refs := obj.GetOwnerReferences()
		if len(refs) != 1 || refs[0].UID != "uid-1" || refs[0].Kind != "JSHook" {
			t.Fatalf("%s: owner references %+v, want the hook", obj.GetName(), refs)
		}
		if obj.GetLabels()[ManagedByLabel] != ManagedBy {
			t.Fatalf("%s: labels %v", obj.GetName(), obj.GetLabels())
		}
	}
	var gotSA corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKey{Namespace: "gojsop-system", Name: sa}, &gotSA); err != nil {
		t.Fatalf("service account: %v", err)
	}
	ownedByHook(&gotSA)
	var role rbacv1.ClusterRole
	if err := c.Get(ctx, client.ObjectKey{Name: sa}, &role); err != nil {
		t.Fatalf("cluster role: %v", err)
	}
	ownedByHook(&role)
	if len(role.Rules) != 1 || role.Rules[0].Resources[0] != "configmaps" {
		t.Fatalf("rules %+v", role.Rules)
	}
	var binding rbacv1.ClusterRoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: sa}, &binding); err != nil {
		t.Fatalf("cluster role binding: %v", err)
	}
	ownedByHook(&binding)
	if binding.RoleRef.Name != sa || len(binding.Subjects) != 1 ||
		binding.Subjects[0].Name != sa || binding.Subjects[0].Namespace != "gojsop-system" {
		t.Fatalf("binding %+v", binding)
	}

	// Changed rights replace the old ones.
	rules = []rbacv1.PolicyRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"list"}}}
	if _, err := m.Ensure(ctx, hook, jsrun.KindJSHook, rules); err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: sa}, &role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 || role.Rules[0].Resources[0] != "deployments" {
		t.Fatalf("rules after change %+v", role.Rules)
	}
}

// kube-access.R12
func TestImpersonatingConfig_ActsAsTheServiceAccount(t *testing.T) {
	base := &rest.Config{Host: "https://example"}
	got := ImpersonatingConfig(base, "gojsop-system", "jshook-sync")
	if got.Impersonate.UserName != "system:serviceaccount:gojsop-system:jshook-sync" {
		t.Fatalf("impersonates %q", got.Impersonate.UserName)
	}
	if base.Impersonate.UserName != "" {
		t.Fatal("the operator's own config must stay unchanged")
	}
}
