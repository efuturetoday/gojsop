package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
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

// workspace.R6
func TestRun_HookChangesTheFakeCluster(t *testing.T) {
	ctx := context.Background()
	m := load(t, "testdata/count-pods")
	pod := func(name string, labels map[string]any) map[string]any {
		return obj("Pod", "default", name, map[string]any{
			"metadata": map[string]any{"name": name, "namespace": "default", "labels": labels},
		})
	}
	tracked := map[string]any{"track": "true"}
	out, err := m.Run(ctx, workspace.Input{
		Cluster: []map[string]any{pod("a", tracked), pod("b", tracked), pod("c", map[string]any{})},
		Event:   &workspace.EventSpec{Object: pod("a", tracked)},
	})
	if err != nil || out.Error != "" {
		t.Fatalf("err = %v, out.Error = %q", err, out.Error)
	}
	var count any
	for _, o := range out.Cluster {
		if o["kind"] == "ConfigMap" {
			count = o["data"].(map[string]any)["count"]
		}
	}
	if count != "2" || len(out.Cluster) != 4 { // event.all() skipped the untracked pod
		t.Fatalf("count = %v, cluster = %v", count, out.Cluster)
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
	out, err = ns.Run(ctx, workspace.Input{
		Cluster: []map[string]any{prod, dev, inNS("prod", "p1"), inNS("dev", "d1")},
		Event:   &workspace.EventSpec{Object: inNS("prod", "p1")},
	})
	if err != nil || out.Error != "" || string(out.Return) != `["p1"]` {
		t.Fatalf("event.all() = %s, err = %v, out.Error = %q", out.Return, err, out.Error)
	}
}

// serve sends the requests to workspace.Serve and returns its answers.
func serve(t *testing.T, requests ...map[string]any) []map[string]any {
	t.Helper()
	var in bytes.Buffer
	for _, r := range requests {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		in.Write(line)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if err := workspace.Serve(context.Background(), &in, &out); err != nil {
		t.Fatal(err)
	}
	answers := make([]map[string]any, 0, len(requests))
	for line := range bytes.SplitSeq(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var a map[string]any
		if err := json.Unmarshal(line, &a); err != nil {
			t.Fatalf("answer %q: %v", line, err)
		}
		answers = append(answers, a)
	}
	if len(answers) != len(requests) {
		t.Fatalf("%d answers for %d requests: %s", len(answers), len(requests), out.String())
	}
	return answers
}

// workspace.R4
func TestServe_AnswersReviewAndHandle(t *testing.T) {
	pod := obj("Pod", "default", "a", map[string]any{
		"metadata": map[string]any{"name": "a", "namespace": "default", "labels": map[string]any{"track": "true"}},
		"spec":     map[string]any{"containers": []any{map[string]any{"name": "app", "image": "nginx:latest"}}},
	})
	ans := serve(t,
		map[string]any{"id": 1, "op": "review", "manifest": "testdata/no-latest/policy.yaml", "input": map[string]any{"object": pod}},
		map[string]any{"id": "two", "op": "handle", "manifest": "testdata/count-pods", "input": map[string]any{"object": pod}, "cluster": []any{pod}},
	)

	review := ans[0]
	result, _ := review["result"].(map[string]any)
	if review["id"] != float64(1) || review["error"] != nil || result["allowed"] != false || result["message"] != "nginx:latest uses :latest" {
		t.Errorf("review answer = %v", review)
	}

	handle := ans[1]
	result, _ = handle["result"].(map[string]any)
	cluster, _ := handle["cluster"].([]any)
	if handle["id"] != "two" || handle["error"] != nil || result["return"] == nil || len(cluster) != 2 {
		t.Errorf("handle answer = %v", handle)
	}
	if handle["console"] == nil {
		t.Errorf("console must be a list, got null")
	}
}

// workspace.R11
func TestServe_SourceFromTheCaller(t *testing.T) {
	bundled := "globalThis.validate = function () { return {allowed: false, message: \"bundled\"}; };"
	tsOnly := writeFiles(t, map[string]string{"policy.yaml": policyHeader, "policy.ts": "export function validate() {}"})
	inline := writeFiles(t, map[string]string{
		"policy.yaml": policyHeader + "  source:\n    inline: \"function validate() { return {allowed: true}; }\"\n",
	})
	review := func(id int, manifest string) map[string]any {
		return map[string]any{"id": id, "op": "review", "manifest": manifest, "source": bundled,
			"input": map[string]any{"object": obj("Pod", "d", "a", nil)}}
	}
	ans := serve(t, review(1, "testdata/no-latest"), review(2, tsOnly), review(3, inline))

	for i := range 2 {
		result, _ := ans[i]["result"].(map[string]any)
		if ans[i]["error"] != nil || result["message"] != "bundled" {
			t.Errorf("answer %d = %v, want the caller's script", i+1, ans[i])
		}
	}
	e, _ := ans[2]["error"].(map[string]any)
	if msg, _ := e["message"].(string); e["kind"] != "input" || !strings.Contains(msg, "keep one") {
		t.Errorf("source and inline: answer = %v", ans[2])
	}
}

// workspace.R5
func TestServe_ErrorsAndLimitsKeepServing(t *testing.T) {
	dir := writeFiles(t, map[string]string{
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
	})
	review := func(id any, name string) map[string]any {
		return map[string]any{"id": id, "op": "review", "manifest": dir, "input": map[string]any{"object": obj("Pod", "d", name, nil)}}
	}
	ans := serve(t,
		review(1, "throw"),
		review(2, "loop"),
		review(3, "memory"),
		map[string]any{"id": 4, "op": "review", "manifest": dir + "/missing.yaml", "input": map[string]any{}},
		map[string]any{"id": 5, "op": "handle", "manifest": dir, "input": map[string]any{}},
		review(6, "fine"),
	)
	for i, want := range []struct{ kind, text string }{
		{"script", "boom"}, {"timeout", "timeout"}, {"memoryLimit", "memory"}, {"input", ""}, {"input", "hook manifest"},
	} {
		e, _ := ans[i]["error"].(map[string]any)
		msg, _ := e["message"].(string)
		if e["kind"] != want.kind || !strings.Contains(msg, want.text) || ans[i]["id"] != float64(i+1) {
			t.Errorf("answer %d = %v, want kind %s containing %q", i+1, ans[i], want.kind, want.text)
		}
	}
	// After every failure the process still answers.
	last, _ := ans[5]["result"].(map[string]any)
	if ans[5]["error"] != nil || last["allowed"] != true {
		t.Errorf("last answer = %v", ans[5])
	}
}
