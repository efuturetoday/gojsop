// Package jsaccess gives every JSHook and JSAdmission a ServiceAccount of its
// own with exactly the rights the resource declares, hands out clients that
// act as that ServiceAccount, and checks that whoever creates or changes such
// a resource already holds those rights.
package jsaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sync"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
	"github.com/efuturetoday/gojsop/internal/jsrun"
)

const (
	// ManagedByLabel marks the ServiceAccounts, ClusterRoles and
	// ClusterRoleBindings gojsop creates.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// ManagedBy is the value of ManagedByLabel.
	ManagedBy = "gojsop"

	// maxNameLength is the longest name a ServiceAccount or ClusterRole may
	// have (a DNS subdomain).
	maxNameLength = 253
)

// readVerbs are what a binding needs on the resources it watches.
var readVerbs = []string{"get", "list", "watch"}

// Name is the name of the ServiceAccount, ClusterRole and ClusterRoleBinding
// of a resource: the kind as a prefix, so a JSHook and a JSAdmission of the
// same name never share rights. A name too long for Kubernetes is cut and
// made unique with a hash.
// kube-access.R10
func Name(kind jsrun.Kind, name string) string {
	prefix := "jshook-"
	if kind == jsrun.KindJSAdmission {
		prefix = "jsadmission-"
	}
	n := prefix + name
	if len(n) <= maxNameLength {
		return n
	}
	sum := sha256.Sum256([]byte(n))
	suffix := "-" + hex.EncodeToString(sum[:])[:10]
	return n[:maxNameLength-len(suffix)] + suffix
}

// Username is the user a ServiceAccount authenticates as.
func Username(namespace, serviceAccount string) string {
	return "system:serviceaccount:" + namespace + ":" + serviceAccount
}

// HookRules are the rights of a JSHook: its permissions, plus get, list and
// watch on every resource a binding watches, because the watches run as the
// hook's ServiceAccount.
// kube-access.R11
func HookRules(spec corev1alpha1.JSHookSpec) []rbacv1.PolicyRule {
	rules := permissionRules(spec.Permissions)
	for _, b := range spec.Bindings {
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: append([]string(nil), b.APIGroups...),
			Resources: append([]string(nil), b.Resources...),
			Verbs:     append([]string(nil), readVerbs...),
		})
	}
	return rules
}

// AdmissionRules are the rights of a JSAdmission: its permissions. The CRD
// allows only read verbs there.
func AdmissionRules(spec corev1alpha1.JSAdmissionSpec) []rbacv1.PolicyRule {
	return permissionRules(spec.Permissions)
}

func permissionRules(perms []corev1alpha1.Permission) []rbacv1.PolicyRule {
	rules := make([]rbacv1.PolicyRule, 0, len(perms))
	for _, p := range perms {
		verbs := make([]string, len(p.Verbs))
		for i, v := range p.Verbs {
			verbs[i] = string(v)
		}
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: append([]string(nil), p.APIGroups...),
			Resources: append([]string(nil), p.Resources...),
			Verbs:     verbs,
		})
	}
	return rules
}

// Manager creates the ServiceAccounts and their rights, and hands out
// clients that act as one of them.
type Manager struct {
	// Client is the operator's own client.
	Client client.Client
	// Scheme knows the gojsop kinds, for owner references.
	Scheme *runtime.Scheme
	// Namespace is the operator's namespace, home of every ServiceAccount.
	Namespace string
	// Config is the operator's rest config; clients for a ServiceAccount
	// impersonate it on top of this config.
	Config *rest.Config

	mu      sync.Mutex
	clients map[string]dynamic.Interface
}

// The operator manages and impersonates ServiceAccounts in its own namespace
// only: config/rbac/manager_namespace_role.yaml (a Role, not generated, so
// it does not share the name of the generated ClusterRole).
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create;update;patch;delete;escalate;bind
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

// Ensure creates or updates the ServiceAccount, ClusterRole and
// ClusterRoleBinding of owner and returns the ServiceAccount's name. All
// three carry an owner reference to owner, so the garbage collector removes
// them together with the resource.
// kube-access.R10
func (m *Manager) Ensure(ctx context.Context, owner client.Object, kind jsrun.Kind, rules []rbacv1.PolicyRule) (string, error) {
	name := Name(kind, owner.GetName())
	labels := map[string]string{ManagedByLabel: ManagedBy}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, m.Client, sa, func() error {
		sa.Labels = merge(sa.Labels, labels)
		return controllerutil.SetControllerReference(owner, sa, m.Scheme)
	}); err != nil {
		return "", fmt.Errorf("service account %s: %w", name, err)
	}

	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, m.Client, role, func() error {
		role.Labels = merge(role.Labels, labels)
		role.Rules = rules
		return controllerutil.SetControllerReference(owner, role, m.Scheme)
	}); err != nil {
		return "", fmt.Errorf("cluster role %s: %w", name, err)
	}

	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, m.Client, binding, func() error {
		binding.Labels = merge(binding.Labels, labels)
		// roleRef cannot change after create; it never does, the name is fixed.
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}
		binding.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: m.Namespace}}
		return controllerutil.SetControllerReference(owner, binding, m.Scheme)
	}); err != nil {
		return "", fmt.Errorf("cluster role binding %s: %w", name, err)
	}
	return name, nil
}

// ClientFor returns a dynamic client that acts as the ServiceAccount sa of
// the operator's namespace. Clients are cached per ServiceAccount.
// kube-access.R12
func (m *Manager) ClientFor(sa string) (dynamic.Interface, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[sa]; ok {
		return c, nil
	}
	c, err := dynamic.NewForConfig(ImpersonatingConfig(m.Config, m.Namespace, sa))
	if err != nil {
		return nil, err
	}
	if m.clients == nil {
		m.clients = make(map[string]dynamic.Interface)
	}
	m.clients[sa] = c
	return c, nil
}

// ImpersonatingConfig is cfg acting as the ServiceAccount sa in namespace.
func ImpersonatingConfig(cfg *rest.Config, namespace, sa string) *rest.Config {
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{UserName: Username(namespace, sa)}
	return c
}

func merge(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	maps.Copy(dst, src)
	return dst
}
