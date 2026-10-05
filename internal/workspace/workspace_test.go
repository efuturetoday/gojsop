package workspace_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/efuturetoday/gojsop/internal/workspace"
)

// writeFiles lays out a workspace in a temporary directory.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func load(t *testing.T, path string) *workspace.Manifest {
	t.Helper()
	m, err := workspace.Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	return m
}

func obj(kind, ns, name string, extra map[string]any) map[string]any {
	apiVersion := "v1"
	if kind == "Deployment" {
		apiVersion = "apps/v1"
	}
	meta := map[string]any{"name": name}
	if ns != "" {
		meta["namespace"] = ns
	}
	o := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
	maps.Copy(o, extra)
	return o
}

const policyHeader = `apiVersion: core.gojsop.io/v1alpha1
kind: JSAdmission
metadata: {name: p}
spec:
  rules: [{apiGroups: [""], apiVersions: [v1], resources: [pods], operations: [CREATE]}]
`

const hookHeader = `apiVersion: core.gojsop.io/v1alpha1
kind: JSHook
metadata: {name: h}
spec:
  bindings: [{name: pods, apiGroups: [""], apiVersions: [v1], resources: [pods]}]
`

// workspace.R1
func TestRun_UsesTheOperatorsEngineAndKubeSurface(t *testing.T) {
	ctx := context.Background()

	// A mutating policy: kube.get reads the fake cluster, console reaches the
	// output, the patch is what the operator would send.
	m := load(t, "testdata/add-team-label")
	out, err := m.Run(ctx, workspace.Input{
		Cluster: []map[string]any{obj("Namespace", "", "shop", map[string]any{
			"metadata": map[string]any{"name": "shop", "labels": map[string]any{"team": "checkout"}},
		})},
		Request: &workspace.RequestSpec{Object: obj("Deployment", "shop", "api", nil)},
	})
	if err != nil {
		t.Fatal(err)
	}
	labels, _ := out.PatchedObject["metadata"].(map[string]any)["labels"].(map[string]any)
	if out.Error != "" || labels["team"] != "checkout" {
		t.Fatalf("patched labels = %v, error = %q", labels, out.Error)
	}
	if len(out.Console) != 1 || out.Console[0].Text != "team is checkout" {
		t.Fatalf("console = %v", out.Console)
	}

	// A hook: the write half of kube.* and event.all() are bound.
	h := load(t, "testdata/count-pods")
	pod := obj("Pod", "default", "a", map[string]any{
		"metadata": map[string]any{"name": "a", "namespace": "default", "labels": map[string]any{"track": "true"}},
	})
	hout, err := h.Run(ctx, workspace.Input{Event: &workspace.EventSpec{Object: pod}, Cluster: []map[string]any{pod}})
	if err != nil {
		t.Fatal(err)
	}
	if hout.Error != "" || string(hout.Return) != `{"counted":1}` {
		t.Fatalf("hook return = %s, error = %q", hout.Return, hout.Error)
	}

	// A policy gets the read-only surface, as in the operator.
	p := load(t, writeFiles(t, map[string]string{
		"policy.yaml": policyHeader + "  source:\n    inline: |\n      function validate(r) { kube.apply({apiVersion: 'v1', kind: 'ConfigMap', metadata: {name: 'x'}}); return {allowed: true}; }\n",
	}))
	pout, err := p.Run(ctx, workspace.Input{Request: &workspace.RequestSpec{Object: obj("Pod", "default", "a", nil)}})
	if err != nil {
		t.Fatal(err)
	}
	if pout.Error == "" {
		t.Fatal("kube.apply worked in a policy: the write surface leaked")
	}
}

// workspace.R2
func TestLoad_ScriptInlineOrBesideTheManifest(t *testing.T) {
	inline := load(t, writeFiles(t, map[string]string{
		"policy.yaml": policyHeader + "  source:\n    inline: \"function validate() { return {allowed: true}; }\"\n",
		"other.js":    "// ignored: inline wins",
	}))
	if !strings.Contains(string(inline.Source), "validate") {
		t.Fatalf("inline source = %q", inline.Source)
	}

	beside := load(t, writeFiles(t, map[string]string{
		"hook.yaml": hookHeader,
		"main.js":   "function handle() {}",
	}))
	if string(beside.Source) != "function handle() {}" || beside.Hook == nil {
		t.Fatalf("beside source = %q, hook = %v", beside.Source, beside.Hook)
	}

	if _, err := workspace.Load(writeFiles(t, map[string]string{"hook.yaml": hookHeader})); err == nil {
		t.Fatal("no script: want an error")
	}
	if _, err := workspace.Load(writeFiles(t, map[string]string{
		"hook.yaml": hookHeader, "a.js": "1", "b.js": "2",
	})); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatalf("two scripts: err = %v", err)
	}
}

// workspace.R3
func TestFakeCluster_EnforcesPermissions(t *testing.T) {
	m := load(t, writeFiles(t, map[string]string{
		"hook.yaml": hookHeader + `  permissions: [{apiGroups: [""], resources: [configmaps], verbs: [get, create]}]
  source:
    inline: |
      function handle(event) {
        const r = {};
        const tryIt = (k, f) => { try { f(); r[k] = "ok"; } catch (e) { r[k] = String(e.message ?? e); } };
        tryIt("getConfigMap", () => kube.get({apiVersion: "v1", kind: "ConfigMap", namespace: "d", name: "x"}));
        tryIt("deleteConfigMap", () => kube.delete({apiVersion: "v1", kind: "ConfigMap", namespace: "d", name: "x"}));
        tryIt("getSecret", () => kube.get({apiVersion: "v1", kind: "Secret", namespace: "d", name: "x"}));
        tryIt("listPods", () => kube.list({apiVersion: "v1", kind: "Pod", namespace: "d"}));
        return r;
      }
`,
	}))
	out, err := m.Run(context.Background(), workspace.Input{
		Event: &workspace.EventSpec{Object: obj("Pod", "d", "p", nil)},
	})
	if err != nil || out.Error != "" {
		t.Fatalf("run: err = %v, out.Error = %q", err, out.Error)
	}
	ret := string(out.Return)
	for _, want := range []string{`"getConfigMap":"ok"`, `"listPods":"ok"`} { // the binding grants read
		if !strings.Contains(ret, want) {
			t.Errorf("want %s in %s", want, ret)
		}
	}
	for _, k := range []string{"deleteConfigMap", "getSecret"} {
		if !strings.Contains(ret, `"`+k+`":"`) || strings.Contains(ret, `"`+k+`":"ok"`) || !strings.Contains(strings.ToLower(ret), "forbidden") {
			t.Errorf("%s should be Forbidden: %s", k, ret)
		}
	}
}

// workspace.R4
func TestCase_PolicyExpectations(t *testing.T) {
	ctx := context.Background()
	deny := load(t, "testdata/no-latest")
	latest := &workspace.RequestSpec{Object: obj("Pod", "d", "p", map[string]any{
		"spec": map[string]any{"containers": []any{map[string]any{"name": "a", "image": "nginx:latest"}}},
	})}
	run := func(m *workspace.Manifest, c *workspace.Case) []string {
		t.Helper()
		diffs, err := m.RunCase(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		return diffs
	}
	no := false
	for name, tc := range map[string]struct {
		expect workspace.Expect
		fails  bool
	}{
		"exact message":      {workspace.Expect{Allowed: &no, Message: "nginx:latest uses :latest"}, false},
		"regex message":      {workspace.Expect{Allowed: &no, Message: "/^nginx.*:latest$/"}, false},
		"wrong message":      {workspace.Expect{Allowed: &no, Message: "nope"}, true},
		"wrong regex":        {workspace.Expect{Allowed: &no, Message: "/pinned/"}, true},
		"allowed by default": {workspace.Expect{}, true},
		"warnings count":     {workspace.Expect{Allowed: &no, Warnings: []string{"x"}}, true},
	} {
		diffs := run(deny, &workspace.Case{Request: latest, Expect: tc.expect})
		if (len(diffs) > 0) != tc.fails {
			t.Errorf("%s: diffs = %v, want failure = %t", name, diffs, tc.fails)
		}
	}

	// A mutating policy: the expected object is a subset of the patched one.
	mut := load(t, "testdata/add-team-label")
	req := &workspace.RequestSpec{Object: obj("Deployment", "shop", "api", map[string]any{
		"spec": map[string]any{"replicas": 2},
	})}
	ok := &workspace.Case{Request: req, Expect: workspace.Expect{Object: map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"team": "unowned"}},
		"spec":     map[string]any{"replicas": 2},
	}}}
	if diffs := run(mut, ok); len(diffs) != 0 {
		t.Errorf("subset should hold: %v", diffs)
	}
	bad := &workspace.Case{Request: req, Expect: workspace.Expect{Object: map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"team": "other"}},
	}}}
	diffs := run(mut, bad)
	if len(diffs) != 1 || !strings.Contains(diffs[0], `object.metadata.labels.team: want "other", got "unowned"`) {
		t.Errorf("diff = %v", diffs)
	}
}

// workspace.R5
func TestCase_ErrorsAndLimits(t *testing.T) {
	ctx := context.Background()
	m := load(t, writeFiles(t, map[string]string{
		"policy.yaml": policyHeader + `  limits: {memoryMB: 4, timeoutSeconds: 1}
  source:
    inline: |
      function validate(req) {
        const mode = req.object.metadata.name;
        if (mode === "throw") { throw new Error("boom"); }
        if (mode === "loop") { for (;;) {} }
        if (mode === "memory") { const a = []; for (;;) { a.push(new Array(100000).fill(1)); } }
        return {allowed: true};
      }
`,
	}))
	for _, tc := range []struct{ name, want string }{
		{"throw", "/boom/"},
		{"loop", "/timeout/"},
		{"memory", "/memory/"},
	} {
		c := &workspace.Case{
			Request: &workspace.RequestSpec{Object: obj("Pod", "d", tc.name, nil)},
			Expect:  workspace.Expect{Error: tc.want},
		}
		if diffs, err := m.RunCase(ctx, c); err != nil || len(diffs) != 0 {
			t.Errorf("%s: expected error %s: diffs = %v, err = %v", tc.name, tc.want, diffs, err)
		}
		// Without the expectation the same call fails the case.
		c.Expect = workspace.Expect{}
		if diffs, err := m.RunCase(ctx, c); err != nil || len(diffs) != 1 || !strings.HasPrefix(diffs[0], "unexpected error") {
			t.Errorf("%s: unexpected error not reported: diffs = %v, err = %v", tc.name, diffs, err)
		}
	}
	// A call that succeeds does not meet an error expectation.
	c := &workspace.Case{
		Request: &workspace.RequestSpec{Object: obj("Pod", "d", "fine", nil)},
		Expect:  workspace.Expect{Error: "/boom/"},
	}
	if diffs, _ := m.RunCase(ctx, c); len(diffs) != 1 {
		t.Errorf("success where an error was expected: %v", diffs)
	}
}

// workspace.R6
func TestCase_HookChangesTheFakeCluster(t *testing.T) {
	ctx := context.Background()
	m := load(t, "testdata/count-pods")
	pod := func(name string, labels map[string]any) map[string]any {
		return obj("Pod", "default", name, map[string]any{
			"metadata": map[string]any{"name": name, "namespace": "default", "labels": labels},
		})
	}
	tracked := map[string]any{"track": "true"}
	cm := func(count string) map[string]any {
		return obj("ConfigMap", "default", "pod-count", map[string]any{"data": map[string]any{"count": count}})
	}
	c := &workspace.Case{
		Cluster: []map[string]any{pod("a", tracked), pod("b", tracked), pod("c", map[string]any{})},
		Event:   &workspace.EventSpec{Object: pod("a", tracked)},
		Expect:  workspace.Expect{Cluster: []map[string]any{cm("2")}},
	}
	if diffs, err := m.RunCase(ctx, c); err != nil || len(diffs) != 0 {
		t.Fatalf("diffs = %v, err = %v", diffs, err)
	}
	c.Expect.Cluster = []map[string]any{cm("3"), obj("ConfigMap", "default", "absent", nil)}
	diffs, err := m.RunCase(ctx, c)
	if err != nil || len(diffs) != 2 ||
		!strings.Contains(diffs[0], `data.count: want "3", got "2"`) || !strings.Contains(diffs[1], "absent is missing") {
		t.Fatalf("diffs = %v, err = %v", diffs, err)
	}

	// The binding's namespaceSelector reads the labels of Namespace objects.
	ns := load(t, writeFiles(t, map[string]string{
		"hook.yaml": `apiVersion: core.gojsop.io/v1alpha1
kind: JSHook
metadata: {name: ns}
spec:
  bindings:
    - name: pods
      apiGroups: [""]
      apiVersions: [v1]
      resources: [pods]
      namespaceSelector: {matchLabels: {env: prod}}
  source:
    inline: "function handle(e) { return e.all().map(p => p.metadata.name); }"
`}))
	prod := obj("Namespace", "", "prod", map[string]any{"metadata": map[string]any{"name": "prod", "labels": map[string]any{"env": "prod"}}})
	dev := obj("Namespace", "", "dev", nil)
	inNS := func(ns, name string) map[string]any { return obj("Pod", ns, name, nil) }
	out, err := ns.Run(ctx, workspace.Input{
		Cluster: []map[string]any{prod, dev, inNS("prod", "p1"), inNS("dev", "d1")},
		Event:   &workspace.EventSpec{Object: inNS("prod", "p1")},
	})
	if err != nil || out.Error != "" || string(out.Return) != `["p1"]` {
		t.Fatalf("event.all() = %s, err = %v, out.Error = %q", out.Return, err, out.Error)
	}
}
