package jslifecycle

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
	"github.com/efuturetoday/gojsop/internal/jsrun"
)

// The CRD enum of recentRestarts[].reason lists exactly the reasons the
// runner sets, in both CRDs.
//
// api-design.R3
// status-conditions.R4
func TestRecoveryReasons_MatchCRDEnum(t *testing.T) {
	want := []string{
		string(jsrun.ReasonSourceChanged),
		string(jsrun.ReasonLimitsChanged),
		string(jsrun.ReasonManual),
	}
	slices.Sort(want)
	for _, file := range []string{"core.gojsop.io_jshooks.yaml", "core.gojsop.io_jsadmissions.yaml"} {
		t.Run(file, func(t *testing.T) {
			f, err := os.Open(filepath.Join("..", "..", "config", "crd", "bases", file))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			var crd map[string]any
			if err := yaml.NewYAMLOrJSONDecoder(f, 4096).Decode(&crd); err != nil {
				t.Fatal(err)
			}
			got := enumOf(crd, "spec", "versions", 0, "schema", "openAPIV3Schema", "properties", "status",
				"properties", "script", "properties", "recentRestarts", "items", "properties", "reason", "enum")
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("CRD enum %v, want the jsrun reasons %v", got, want)
			}
		})
	}
}

// enumOf walks maps by key and lists by index and returns the strings found.
func enumOf(v any, path ...any) []string {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			l, _ := v.([]any)
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// A success clears the error of a failure once; a steady success leaves the
// status as it is, so the reconcile does not write status and trigger itself.
//
// status-conditions.R5
func TestReconcileSucceeded_ClearsErrorOnceThenKeepsStatus(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	failed := metav1.NewTime(t0)

	got := ReconcileSucceeded(&corev1alpha1.JSReconcileStatus{Time: &failed, Error: "boom"}, t1)
	if got.Error != "" || got.Time == nil || !got.Time.Time.Equal(t1) {
		t.Fatalf("after a failure: %+v, want no error and time %v", got, t1)
	}
	if again := ReconcileSucceeded(got, t1.Add(time.Hour)); again != got {
		t.Fatalf("a steady success changed the status: %+v", again)
	}
	if first := ReconcileSucceeded(nil, t1); first == nil || first.Time == nil || first.Error != "" {
		t.Fatalf("first success: %+v, want time and no error", first)
	}
}
