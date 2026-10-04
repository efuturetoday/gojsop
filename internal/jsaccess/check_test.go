package jsaccess

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
)

// reviewer allows exactly the "verb resource" pairs in held and records
// every question.
type reviewer struct {
	held  map[string]bool
	asked []string
	user  string
}

func (r *reviewer) review(_ context.Context, sar *authorizationv1.SubjectAccessReview) (bool, error) {
	a := sar.Spec.ResourceAttributes
	label := a.Verb + " " + groupResource(a.Group, a.Resource)
	r.asked = append(r.asked, label)
	r.user = sar.Spec.User
	return r.held[label], nil
}

func hookWith(perms ...corev1alpha1.Permission) *corev1alpha1.JSHook {
	return &corev1alpha1.JSHook{
		ObjectMeta: metav1.ObjectMeta{Name: "sync"},
		Spec: corev1alpha1.JSHookSpec{
			Source: corev1alpha1.JSSource{Inline: "function handle() {}"},
			Bindings: []corev1alpha1.HookBinding{{
				Name:         "cms",
				ResourceRule: corev1alpha1.ResourceRule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"}},
			}},
			Permissions: perms,
		},
	}
}

func request(t *testing.T, op admissionv1.Operation, obj, old runtime.Object) admission.Request {
	t.Helper()
	raw := func(o runtime.Object) runtime.RawExtension {
		if o == nil {
			return runtime.RawExtension{}
		}
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		return runtime.RawExtension{Raw: b}
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op,
		Kind:      metav1.GroupVersionKind{Group: "core.gojsop.io", Version: "v1alpha1", Kind: "JSHook"},
		Name:      "sync",
		UserInfo:  authenticationv1.UserInfo{Username: "alice", Groups: []string{"team-a"}},
		Object:    raw(obj),
		OldObject: raw(old),
	}}
}

var writeSecrets = corev1alpha1.Permission{
	APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []corev1alpha1.PermissionVerb{"get", "update"},
}

// kube-access.R13
func TestChecker_DeniesRightsTheUserDoesNotHold(t *testing.T) {
	r := &reviewer{held: map[string]bool{"get configmaps": true, "list configmaps": true, "watch configmaps": true, "get secrets": true}}
	c := &Checker{Review: r.review}

	resp := c.Handle(context.Background(), request(t, admissionv1.Create, hookWith(writeSecrets), nil))
	if resp.Allowed {
		t.Fatal("a hook that may update secrets must be denied to a user who may not")
	}
	if msg := resp.Result.Message; !strings.Contains(msg, "update secrets") || strings.Contains(msg, "get secrets") {
		t.Fatalf("message must name exactly the missing right: %q", msg)
	}
	if r.user != "alice" {
		t.Fatalf("asked about user %q, want the requester", r.user)
	}
}

// kube-access.R13
func TestChecker_AllowsWhenTheUserHoldsEveryRight(t *testing.T) {
	r := &reviewer{held: map[string]bool{
		"get configmaps": true, "list configmaps": true, "watch configmaps": true,
		"get secrets": true, "update secrets": true,
	}}
	c := &Checker{Review: r.review}
	resp := c.Handle(context.Background(), request(t, admissionv1.Create, hookWith(writeSecrets), nil))
	if !resp.Allowed {
		t.Fatalf("denied: %v", resp.Result.Message)
	}
	// The watch of a binding needs read rights too, so they are checked.
	for _, want := range []string{"list configmaps", "watch configmaps"} {
		found := false
		for _, a := range r.asked {
			found = found || a == want
		}
		if !found {
			t.Fatalf("did not check %q; asked %v", want, r.asked)
		}
	}
}

// kube-access.R13
func TestChecker_UpdateOfTheScriptChecksEveryRight(t *testing.T) {
	r := &reviewer{held: map[string]bool{"get configmaps": true, "list configmaps": true, "watch configmaps": true}}
	c := &Checker{Review: r.review}

	old := hookWith(writeSecrets)
	changed := hookWith(writeSecrets)
	changed.Spec.Source.Inline = "function handle() { /* new code */ }"
	if resp := c.Handle(context.Background(), request(t, admissionv1.Update, changed, old)); resp.Allowed {
		t.Fatal("changing the script of a hook must need the hook's rights, or anyone could reuse them")
	}

	labelsOnly := hookWith(writeSecrets)
	labelsOnly.Labels = map[string]string{"team": "a"}
	r.asked = nil
	if resp := c.Handle(context.Background(), request(t, admissionv1.Update, labelsOnly, old)); !resp.Allowed {
		t.Fatalf("a metadata-only change must pass: %v", resp.Result.Message)
	}
	if len(r.asked) != 0 {
		t.Fatalf("a metadata-only change asked %v", r.asked)
	}
}
