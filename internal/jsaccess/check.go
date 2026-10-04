package jsaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
)

// CheckPath is where the operator serves the access check.
const CheckPath = "/validate-gojsop-access"

// Reviewer answers whether the subject of sar may do what sar asks.
type Reviewer func(ctx context.Context, sar *authorizationv1.SubjectAccessReview) (bool, error)

// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// ClientReviewer asks the apiserver through c.
func ClientReviewer(c client.Client) Reviewer {
	return func(ctx context.Context, sar *authorizationv1.SubjectAccessReview) (bool, error) {
		if err := c.Create(ctx, sar); err != nil {
			return false, err
		}
		return sar.Status.Allowed, nil
	}
}

// +kubebuilder:webhook:path=/validate-gojsop-access,mutating=false,failurePolicy=fail,sideEffects=None,groups=core.gojsop.io,resources=jshooks;jsadmissions,verbs=create;update,versions=v1alpha1,name=access.gojsop.io,admissionReviewVersions=v1

// Checker admits a JSHook or JSAdmission only when the user who creates or
// changes it holds every right the resource's ServiceAccount would get.
// Kubernetes applies the same rule to Roles: nobody hands out a right they
// do not have. Without it, anyone who may write a JSHook could reach
// everything the operator may grant.
// kube-access.R13
type Checker struct {
	Review Reviewer
}

// Handle implements admission.Handler.
func (c *Checker) Handle(ctx context.Context, req admission.Request) admission.Response {
	newRules, newSpec, err := rulesOf(req.Kind.Kind, req.Object.Raw)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if len(req.OldObject.Raw) > 0 {
		_, oldSpec, err := rulesOf(req.Kind.Kind, req.OldObject.Raw)
		if err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		// Only metadata changed (labels, the restart annotation): the
		// script and its rights stay as they were.
		if equality.Semantic.DeepEqual(oldSpec, newSpec) {
			return admission.Allowed("")
		}
	}
	missing, err := c.missing(ctx, req.UserInfo, newRules)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if len(missing) > 0 {
		return admission.Denied(fmt.Sprintf(
			"%s %q would get rights you do not have: %s",
			req.Kind.Kind, req.Name, strings.Join(missing, ", ")))
	}
	return admission.Allowed("")
}

// rulesOf decodes a JSHook or JSAdmission and returns its rules and spec.
func rulesOf(kind string, raw []byte) ([]rbacv1.PolicyRule, any, error) {
	switch kind {
	case "JSHook":
		var h corev1alpha1.JSHook
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, nil, err
		}
		return HookRules(h.Spec), h.Spec, nil
	case "JSAdmission":
		var a corev1alpha1.JSAdmission
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, nil, err
		}
		return AdmissionRules(a.Spec), a.Spec, nil
	}
	return nil, nil, fmt.Errorf("unexpected kind %q", kind)
}

// missing lists every verb on every resource of rules that user may not use,
// as "verb group/resource".
func (c *Checker) missing(ctx context.Context, user authenticationv1.UserInfo, rules []rbacv1.PolicyRule) ([]string, error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for k, v := range user.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range rules {
		for _, group := range r.APIGroups {
			for _, res := range r.Resources {
				resource, sub, _ := strings.Cut(res, "/")
				for _, verb := range r.Verbs {
					label := verb + " " + groupResource(group, res)
					if seen[label] {
						continue
					}
					seen[label] = true
					ok, err := c.Review(ctx, &authorizationv1.SubjectAccessReview{
						Spec: authorizationv1.SubjectAccessReviewSpec{
							User:   user.Username,
							UID:    user.UID,
							Groups: user.Groups,
							Extra:  extra,
							ResourceAttributes: &authorizationv1.ResourceAttributes{
								Group:       group,
								Resource:    resource,
								Subresource: sub,
								Verb:        verb,
							},
						},
					})
					if err != nil {
						return nil, fmt.Errorf("access review for %s: %w", label, err)
					}
					if !ok {
						out = append(out, label)
					}
				}
			}
		}
	}
	return out, nil
}

// groupResource spells a resource the way kubectl does: "configmaps",
// "deployments.apps", "deployments.apps/scale".
func groupResource(group, resource string) string {
	if group == "" {
		return resource
	}
	res, sub, ok := strings.Cut(resource, "/")
	if ok {
		return res + "." + group + "/" + sub
	}
	return res + "." + group
}
