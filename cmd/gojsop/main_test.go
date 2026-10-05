package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const workspaceDir = "../../internal/workspace/testdata"

// workspace.R7
func TestTestCommand_ExitCodeAndOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := execute(context.Background(), []string{"test", workspaceDir}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d\n%s%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{
		"ok   " + workspaceDir + "/no-latest denies :latest",
		"ok   " + workspaceDir + "/count-pods counts only the pods its binding selects",
		"6 passed, 0 failed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}

	// One failing case: one FAIL line with a diff, exit 1, the others still run.
	dir := t.TempDir()
	policy, err := os.ReadFile(workspaceDir + "/no-latest/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile(workspaceDir + "/no-latest/policy.js")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"policy.yaml": string(policy),
		"policy.js":   string(js),
		"tests/good.yaml": `name: good
request: {object: {apiVersion: v1, kind: Pod, metadata: {name: p}, spec: {containers: [{name: a, image: "x:1"}]}}}
expect: {allowed: true}
`,
		"tests/bad.yaml": `name: bad
request: {object: {apiVersion: v1, kind: Pod, metadata: {name: p}, spec: {containers: [{name: a, image: "x:latest"}]}}}
expect: {allowed: true}
`,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	if code := execute(context.Background(), []string{"test", dir}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out.String())
	}
	for _, want := range []string{
		"ok   " + dir + " good", "FAIL " + dir + " bad", "allowed: want true, got false", "1 passed, 1 failed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
}

// workspace.R8
func TestRun_TraceShowsKubeCalls(t *testing.T) {
	event := filepath.Join(t.TempDir(), "event.yaml")
	const pod = "object: {apiVersion: v1, kind: Pod, metadata: {name: a, namespace: default, labels: {track: \"true\"}}}\n"
	if err := os.WriteFile(event, []byte(pod), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	args := []string{"run", workspaceDir + "/count-pods", "--event", event, "--trace"}
	if code := execute(context.Background(), args, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d\n%s%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{
		"kube get v1/configmaps default/pod-count -> ",
		"kube create v1/configmaps default/pod-count -> ok",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("missing %q in trace:\n%s", want, errOut.String())
		}
	}
	if !strings.Contains(out.String(), `"counted"`) || strings.Contains(out.String(), "kube get") {
		t.Errorf("stdout is the JSON result only:\n%s", out.String())
	}

	// A call without the right shows its Forbidden in the trace.
	out.Reset()
	errOut.Reset()
	dir := t.TempDir()
	hook := `apiVersion: core.gojsop.io/v1alpha1
kind: JSHook
metadata: {name: h}
spec:
  bindings: [{name: pods, apiGroups: [""], apiVersions: [v1], resources: [pods]}]
  source:
    inline: "function handle() { kube.get({apiVersion: 'v1', kind: 'Secret', namespace: 'default', name: 's'}); }"
`
	if err := os.WriteFile(filepath.Join(dir, "hook.yaml"), []byte(hook), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := execute(context.Background(), []string{"run", dir, "--event", event, "--trace"}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1 (the script threw)\n%s%s", code, out.String(), errOut.String())
	}
	trace := errOut.String()
	if !strings.Contains(trace, "kube get v1/secrets default/s -> ") || !strings.Contains(trace, "forbidden") {
		t.Errorf("trace:\n%s", errOut.String())
	}
}
