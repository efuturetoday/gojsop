package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
	"github.com/efuturetoday/gojsop/internal/jsaccess"
	"github.com/efuturetoday/gojsop/internal/jsadmission"
	"github.com/efuturetoday/gojsop/internal/jsengine/kubehost"
	"github.com/efuturetoday/gojsop/internal/jshook"
	"github.com/efuturetoday/gojsop/internal/jslog"
	"github.com/efuturetoday/gojsop/internal/jsregistry"
	"github.com/efuturetoday/gojsop/internal/jsrun"
	"github.com/efuturetoday/gojsop/internal/jssource"
)

// buildTimeout bounds how long the CLI waits for the registry to prepare the
// script; the script's own limits apply inside.
const buildTimeout = time.Minute

// RequestSpec is the admission request of `gojsop run
// --request`. Kind, resource, name and namespace come from the object unless
// given.
type RequestSpec struct {
	Operation string               `json:"operation,omitempty"`
	Object    map[string]any       `json:"object,omitempty"`
	OldObject map[string]any       `json:"oldObject,omitempty"`
	UserInfo  jsadmission.UserInfo `json:"userInfo,omitempty"`
	Namespace string               `json:"namespace,omitempty"`
	Name      string               `json:"name,omitempty"`
	DryRun    bool                 `json:"dryRun,omitempty"`
}

// EventSpec is the hook event of `gojsop run --event`. Binding
// defaults to the first binding that watches the object's kind.
type EventSpec struct {
	Type    string         `json:"type,omitempty"`
	Object  map[string]any `json:"object"`
	Binding string         `json:"binding,omitempty"`
	Initial bool           `json:"initial,omitempty"`
}

// Input is one call: a request for a policy or an event for a hook, on a
// cluster of the given objects.
type Input struct {
	Request *RequestSpec
	Event   *EventSpec
	Cluster []map[string]any
	// Trace, when set, receives one line per kube.* call (workspace.R8).
	Trace io.Writer
}

// Output is what one call produced, as `gojsop run` prints it.
type Output struct {
	// Result is the policy's answer.
	*jsadmission.AdmissionResult
	// PatchedObject is the object after a mutating policy's patch.
	PatchedObject map[string]any `json:"patchedObject,omitempty"`
	// Return is what a hook's handle returned, as JSON.
	Return json.RawMessage `json:"return,omitempty"`
	// Console holds the console lines of the script.
	Console []jslog.Line `json:"console,omitempty"`
	// Cluster is the fake cluster after the call.
	Cluster []map[string]any `json:"cluster,omitempty"`
	// Error is set when the script threw, timed out or hit its memory limit.
	Error string `json:"error,omitempty"`
	// Kind names what Error is: script, timeout or memoryLimit (workspace.R5).
	Kind string `json:"-"`
}

// Error kinds of a failed call, as serve names them.
const (
	KindScript      = "script"
	KindTimeout     = "timeout"
	KindMemoryLimit = "memoryLimit"
	KindInput       = "input"
)

// errBuild marks an error from preparing the script, which is the script's
// fault and not the input's.
var errBuild = errors.New("script does not build")

// Run prepares the script of m as the operator does and runs one call. The
// error is set only when nothing could run (bad input, the script does not
// build); a failing script is Output.Error.
// workspace.R1
func (m *Manifest) Run(ctx context.Context, in Input) (*Output, error) {
	var rules []rbacv1.PolicyRule
	var lim *corev1alpha1.JSLimits
	if m.Hook != nil {
		rules, lim = jsaccess.HookRules(m.Hook.Spec), m.Hook.Spec.Limits
	} else {
		rules, lim = jsaccess.AdmissionRules(m.Policy.Spec), m.Policy.Spec.Limits
	}
	c, err := newCluster(in.Cluster, requestObjects(in), rules, in.Trace)
	if err != nil {
		return nil, err
	}

	factory := kubehost.NewSharedFactory(ctx, c.dyn, c.mapper)
	key := m.Key()
	var host jsrun.Host
	var post jsrun.PostBuildHook
	if m.Hook != nil {
		host, err = factory.ForHook(ctx, key.Name, "")
		post = requireExport("handle")
	} else {
		host, err = factory.ForAdmission(ctx, key.Name, "")
		entry := "validate"
		if m.Mutating() {
			entry = "mutate"
		}
		post = requireExport(entry)
	}
	if err != nil {
		return nil, err
	}

	reg := jsregistry.NewRegistry()
	defer reg.Drop(key)
	if err := prepare(ctx, reg, key, jsrun.Spec{
		Source:     m.Source,
		SourceHash: jssource.Hash(m.Source),
		Limits:     limitsOf(lim),
		Host:       host,
		PostBuild:  post,
	}); err != nil {
		return nil, err
	}

	console := &jslog.Collector{}
	callCtx := jslog.WithSink(ctx, console)
	out := &Output{}
	var res jsrun.Result
	if m.Hook != nil {
		res, err = m.runHook(callCtx, reg, c, in, out)
	} else {
		res, err = m.runPolicy(callCtx, reg, c, in, out)
	}
	if err != nil {
		return nil, err
	}
	out.Console = console.Lines()
	out.Error, out.Kind = describe(res)
	out.Cluster = c.all()
	return out, nil
}

// requestObjects are the objects of the call itself: not in the cluster, but
// their kinds must be known to the mapper.
func requestObjects(in Input) []map[string]any {
	var objs []map[string]any
	if in.Request != nil {
		objs = append(objs, in.Request.Object, in.Request.OldObject)
	}
	if in.Event != nil {
		objs = append(objs, in.Event.Object)
	}
	return slices.DeleteFunc(objs, func(o map[string]any) bool { return o == nil })
}

func gvkOf(o map[string]any) schema.GroupVersionKind {
	return (&unstructured.Unstructured{Object: o}).GroupVersionKind()
}

func (m *Manifest) runPolicy(ctx context.Context, rt jsrun.Runner, c *cluster, in Input, out *Output) (jsrun.Result, error) {
	if in.Request == nil {
		return jsrun.Result{}, errors.New("a policy needs a request")
	}
	req, err := buildRequest(c, in.Request)
	if err != nil {
		return jsrun.Result{}, err
	}
	res, result, err := callPolicy(ctx, rt, m, req)
	if err != nil || res.Outcome != jsrun.OutcomeOK {
		return res, err
	}
	out.AdmissionResult = result
	if m.Mutating() && req.Object != nil {
		patched, err := applyMutation(req.Object, result.ModifiedObject)
		if err != nil {
			return res, err
		}
		out.PatchedObject = patched
	}
	return res, nil
}

func callPolicy(ctx context.Context, rt jsrun.Runner, m *Manifest, req *jsadmission.AdmissionRequest) (jsrun.Result, *jsadmission.AdmissionResult, error) {
	result, res, err := jsadmission.Handle(ctx, rt, m.Key(), req, m.Mutating())
	return res, result, err
}

// applyMutation applies the patch the operator would send for modified onto
// original; no modified object means no change.
func applyMutation(original, modified map[string]any) (map[string]any, error) {
	if modified == nil {
		return original, nil
	}
	raw, err := json.Marshal(original)
	if err != nil {
		return nil, err
	}
	ops, err := jsadmission.CreatePatch(raw, modified)
	if err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return original, nil
	}
	pb, err := json.Marshal(ops)
	if err != nil {
		return nil, err
	}
	p, err := jsonpatch.DecodePatch(pb)
	if err != nil {
		return nil, err
	}
	patched, err := p.Apply(raw)
	if err != nil {
		return nil, fmt.Errorf("apply patch: %w", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(patched, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func buildRequest(c *cluster, rs *RequestSpec) (*jsadmission.AdmissionRequest, error) {
	req := &jsadmission.AdmissionRequest{
		UID:       "gojsop-test",
		Operation: rs.Operation,
		UserInfo:  rs.UserInfo,
		Object:    rs.Object,
		OldObject: rs.OldObject,
		Name:      rs.Name,
		Namespace: rs.Namespace,
		DryRun:    rs.DryRun,
	}
	if req.Operation == "" {
		req.Operation = "CREATE"
	}
	src := rs.Object
	if src == nil {
		src = rs.OldObject
	}
	if src == nil {
		return nil, errors.New("request needs an object or an oldObject")
	}
	u := unstructured.Unstructured{Object: src}
	gvk := u.GroupVersionKind()
	if gvk.Kind == "" {
		return nil, errors.New("request object has no apiVersion or kind")
	}
	req.Kind = jsadmission.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind}
	gvr := gvrOf(gvk)
	if mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err == nil {
		gvr = mapping.Resource
	}
	req.Resource = jsadmission.GroupVersionResource{Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource}
	if req.Name == "" {
		req.Name = u.GetName()
	}
	if req.Namespace == "" {
		req.Namespace = u.GetNamespace()
	}
	return req, nil
}

func (m *Manifest) runHook(ctx context.Context, rt jsrun.Runner, c *cluster, in Input, out *Output) (jsrun.Result, error) {
	if in.Event == nil {
		return jsrun.Result{}, errors.New("a hook needs an event")
	}
	ev, binding, err := buildEvent(c, m.Hook.Spec.Bindings, in.Event)
	if err != nil {
		return jsrun.Result{}, err
	}
	// workspace.R6
	ctx = jshook.WithLister(ctx, c.lister(binding))
	ret, res, err := jshook.Handle(ctx, rt, m.Key(), ev)
	if err == nil && res.Outcome == jsrun.OutcomeOK && ret != "" {
		out.Return = json.RawMessage(ret)
	}
	return res, err
}

func buildEvent(c *cluster, bindings []corev1alpha1.HookBinding, es *EventSpec) (jshook.Event, *corev1alpha1.HookBinding, error) {
	ev := jshook.Event{Binding: es.Binding, Type: es.Type, Object: es.Object, Initial: es.Initial}
	if ev.Type == "" {
		ev.Type = "Added"
	}
	if es.Object == nil {
		return ev, nil, errors.New("event needs an object")
	}
	var binding *corev1alpha1.HookBinding
	for i := range bindings {
		b := &bindings[i]
		if es.Binding != "" && b.Name == es.Binding || es.Binding == "" && c.bindingWatches(b, gvkOf(es.Object)) {
			binding = b
			break
		}
	}
	if binding == nil {
		if es.Binding != "" {
			return ev, nil, fmt.Errorf("event binding %q is not in spec.bindings", es.Binding)
		}
		return ev, nil, fmt.Errorf("no binding watches %s", gvkOf(es.Object).Kind)
	}
	ev.Binding = binding.Name
	return ev, binding, nil
}

// bindingWatches reports whether b watches objects of gvk.
func (c *cluster) bindingWatches(b *corev1alpha1.HookBinding, gvk schema.GroupVersionKind) bool {
	gvr := gvrOf(gvk)
	if mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err == nil {
		gvr = mapping.Resource
	}
	has := func(set []string, v string) bool { return slices.Contains(set, v) || slices.Contains(set, "*") }
	return has(b.APIGroups, gvr.Group) && has(b.APIVersions, gvr.Version) && has(b.Resources, gvr.Resource)
}

// lister is event.all() for binding b: the objects of its resources in the
// fake cluster, filtered by its selectors.
func (c *cluster) lister(b *corev1alpha1.HookBinding) jshook.Lister {
	return func() ([]map[string]any, error) {
		objSel, err := selector(b.ObjectSelector)
		if err != nil {
			return nil, err
		}
		nsSel, err := selector(b.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		nsLabels := map[string]map[string]string{}
		for _, n := range c.list(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}) {
			u := unstructured.Unstructured{Object: n}
			nsLabels[u.GetName()] = u.GetLabels()
		}
		var out []map[string]any
		for _, g := range b.APIGroups {
			for _, v := range b.APIVersions {
				for _, r := range b.Resources {
					for _, o := range c.list(schema.GroupVersionResource{Group: g, Version: v, Resource: r}) {
						u := unstructured.Unstructured{Object: o}
						if !objSel.Matches(labels.Set(u.GetLabels())) {
							continue
						}
						if ns := u.GetNamespace(); ns != "" && !nsSel.Matches(labels.Set(nsLabels[ns])) {
							continue
						}
						out = append(out, o)
					}
				}
			}
		}
		return out, nil
	}
}

// selector is the label selector of s; no selector matches everything.
func selector(s *metav1.LabelSelector) (labels.Selector, error) {
	if s == nil {
		return labels.Everything(), nil
	}
	return metav1.LabelSelectorAsSelector(s)
}

// requireExport is the registry PostBuildHook the controllers use: the
// entry point of the manifest must exist.
func requireExport(name string) jsrun.PostBuildHook {
	return func(_ context.Context, s jsrun.Script) error {
		if !s.HasExport(name) {
			return &jsrun.MissingExportError{Name: name}
		}
		return nil
	}
}

func limitsOf(l *corev1alpha1.JSLimits) jsrun.Limits {
	if l == nil {
		return jsrun.Limits{}
	}
	return jsrun.Limits{MemoryMB: l.MemoryMB, TimeoutSeconds: l.TimeoutSeconds}
}

// prepare asks the registry to build the script and waits for it. The
// operator never waits (js-registry.R15); a one-shot CLI has nothing else to do.
func prepare(ctx context.Context, reg *jsregistry.Registry, key jsrun.Key, spec jsrun.Spec) error {
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	for {
		st := reg.Ensure(key, spec)
		switch st.Phase {
		case jsrun.PhaseReady:
			return nil
		case jsrun.PhaseFailed:
			return fmt.Errorf("prepare script: %w: %w", errBuild, st.Err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("prepare script: %w", ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// describe is the error text and kind of a call that did not return, "" when
// it did.
// workspace.R5
func describe(res jsrun.Result) (text, kind string) {
	switch res.Outcome {
	case jsrun.OutcomeOK:
		return "", ""
	case jsrun.OutcomePanic:
		return fmt.Sprintf("panic: %v", res.Panic), KindScript
	case jsrun.OutcomeMemoryLimit:
		return "memory limit: " + errText(res.Err), KindMemoryLimit
	case jsrun.OutcomeCancelled:
		return "timeout: " + errText(res.Err), KindTimeout
	}
	return errText(res.Err), KindScript
}

func errText(err error) string {
	if err == nil {
		return "call failed"
	}
	return err.Error()
}
