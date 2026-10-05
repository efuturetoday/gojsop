package workspace

import (
	"fmt"
	"io"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// builtinKinds are the kinds a script may use without the case listing an
// object of them.
var builtinKinds = []struct {
	gvk        schema.GroupVersionKind
	namespaced bool
}{
	{schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, true},
	{schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, true},
	{schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, true},
	{schema.GroupVersionKind{Version: "v1", Kind: "Service"}, true},
	{schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}, true},
	{schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"}, true},
	{schema.GroupVersionKind{Version: "v1", Kind: "Endpoints"}, true},
	{schema.GroupVersionKind{Version: "v1", Kind: "ResourceQuota"}, true},
	{schema.GroupVersionKind{Version: "v1", Kind: "LimitRange"}, true},
	{schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, false},
	{schema.GroupVersionKind{Version: "v1", Kind: "Node"}, false},
	{schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolume"}, false},
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, true},
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}, true},
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"}, true},
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, true},
	{schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, true},
	{schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"}, true},
	{schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"}, true},
	{schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}, true},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"}, true},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}, true},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, false},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"}, false},
}

// cluster is the fake cluster of one call: objects in memory behind a
// dynamic client, and the RESTMapper that knows their kinds.
type cluster struct {
	dyn    *dynamicfake.FakeDynamicClient
	mapper *meta.DefaultRESTMapper
	gvrs   map[schema.GroupVersionResource]schema.GroupVersionKind
}

// newCluster seeds a fake cluster with objs; kinds are objects whose kind the
// mapper must know without the cluster holding them. It rejects every client call the
// rules do not allow (workspace.R3) and, when trace is set, prints each one
// (workspace.R8).
func newCluster(objs, kinds []map[string]any, rules []rbacv1.PolicyRule, trace io.Writer) (*cluster, error) {
	c := &cluster{gvrs: map[schema.GroupVersionResource]schema.GroupVersionKind{}}
	versions := make([]schema.GroupVersion, 0, len(builtinKinds))
	for _, b := range builtinKinds {
		versions = append(versions, b.gvk.GroupVersion())
	}
	c.mapper = meta.NewDefaultRESTMapper(versions)
	for _, b := range builtinKinds {
		c.addKind(b.gvk, b.namespaced)
	}
	seed := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		u := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(o)}
		gvk := u.GroupVersionKind()
		if gvk.Kind == "" || gvk.Version == "" {
			return nil, fmt.Errorf("cluster object %q has no apiVersion or kind", u.GetName())
		}
		if u.GetName() == "" {
			return nil, fmt.Errorf("cluster object of kind %s has no metadata.name", gvk.Kind)
		}
		if _, ok := c.gvrs[gvrOf(gvk)]; !ok {
			c.addKind(gvk, u.GetNamespace() != "")
		}
		seed = append(seed, u)
	}
	for _, o := range kinds {
		u := &unstructured.Unstructured{Object: o}
		if gvk := u.GroupVersionKind(); gvk.Kind != "" {
			if _, ok := c.gvrs[gvrOf(gvk)]; !ok {
				c.addKind(gvk, u.GetNamespace() != "")
			}
		}
	}
	listKinds := map[schema.GroupVersionResource]string{}
	for gvr, gvk := range c.gvrs {
		listKinds[gvr] = gvk.Kind + "List"
	}
	c.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, seed...)

	chain := slices.Clone(c.dyn.ReactionChain)
	c.dyn.PrependReactor("*", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		var err error
		handled, ret := false, runtime.Object(nil)
		if !permitted(rules, a) {
			err = forbidden(a)
		} else {
			for _, r := range chain {
				if !r.Handles(a) {
					continue
				}
				if handled, ret, err = r.React(a); handled {
					break
				}
			}
		}
		if trace != nil {
			traceCall(trace, a, err)
		}
		return handled || err != nil, ret, err
	})
	return c, nil
}

func scopeFor(namespaced bool) meta.RESTScope {
	if namespaced {
		return meta.RESTScopeNamespace
	}
	return meta.RESTScopeRoot
}

func (c *cluster) addKind(gvk schema.GroupVersionKind, namespaced bool) {
	c.mapper.Add(gvk, scopeFor(namespaced))
	c.gvrs[gvrOf(gvk)] = gvk
}

// gvrOf guesses the plural the way the fake client's tracker and the mapper do.
func gvrOf(gvk schema.GroupVersionKind) schema.GroupVersionResource {
	gvr, _ := meta.UnsafeGuessKindToResource(gvk)
	return gvr
}

// list returns every object of gvr in all namespaces, bypassing the rights:
// it is the harness reading, not the script.
func (c *cluster) list(gvr schema.GroupVersionResource) []map[string]any {
	gvk, ok := c.gvrs[gvr]
	if !ok {
		return nil
	}
	l, err := c.dyn.Tracker().List(gvr, gvk, "")
	if err != nil {
		return nil
	}
	items, err := meta.ExtractList(l)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, it := range items {
		if u, ok := it.(*unstructured.Unstructured); ok {
			out = append(out, runtime.DeepCopyJSON(u.Object))
		}
	}
	return out
}

// all returns every object of the cluster, ordered by kind, namespace, name.
func (c *cluster) all() []map[string]any {
	var out []map[string]any
	for gvr := range c.gvrs {
		out = append(out, c.list(gvr)...)
	}
	slices.SortFunc(out, func(a, b map[string]any) int {
		return strings.Compare(objectID(a), objectID(b))
	})
	return out
}

// objectID is "apiVersion kind namespace/name", the identity of an object.
func objectID(o map[string]any) string {
	u := unstructured.Unstructured{Object: o}
	return fmt.Sprintf("%s %s %s/%s", u.GetAPIVersion(), u.GetKind(), u.GetNamespace(), u.GetName())
}

// permitted reports whether rules allow the verb of a on its resource.
// workspace.R3
func permitted(rules []rbacv1.PolicyRule, a clienttesting.Action) bool {
	verb := a.GetVerb()
	if verb == "delete-collection" {
		verb = "deletecollection"
	}
	res := a.GetResource()
	full := res.Resource
	if a.GetSubresource() != "" {
		full += "/" + a.GetSubresource()
	}
	has := func(set []string, v string) bool { return slices.Contains(set, v) || slices.Contains(set, "*") }
	for _, r := range rules {
		if has(r.APIGroups, res.Group) && has(r.Resources, full) && has(r.Verbs, verb) {
			return true
		}
	}
	return false
}

func forbidden(a clienttesting.Action) error {
	return apierrors.NewForbidden(a.GetResource().GroupResource(), nameOf(a),
		fmt.Errorf("the script has no right to %s %s: add it to spec.permissions", a.GetVerb(), a.GetResource().Resource))
}

// nameOf is the object name an action targets, "" for a list.
func nameOf(a clienttesting.Action) string {
	if n, ok := a.(interface{ GetName() string }); ok {
		return n.GetName()
	}
	if o, ok := a.(interface{ GetObject() runtime.Object }); ok {
		if m, err := meta.Accessor(o.GetObject()); err == nil {
			return m.GetName()
		}
	}
	return ""
}

// traceCall prints one kube.* call: verb, resource, namespace/name, answer.
// workspace.R8
func traceCall(w io.Writer, a clienttesting.Action, err error) {
	res := a.GetResource()
	gvr := res.Version + "/" + res.Resource
	if res.Group != "" {
		gvr = res.Group + "/" + gvr
	}
	answer := "ok"
	if err != nil {
		answer = err.Error()
	}
	_, _ = fmt.Fprintf(w, "kube %s %s %s/%s -> %s\n", a.GetVerb(), gvr, a.GetNamespace(), nameOf(a), answer)
}
