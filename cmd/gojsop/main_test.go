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
