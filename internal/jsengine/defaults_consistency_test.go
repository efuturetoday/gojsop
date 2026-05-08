package jsengine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestDefaultLimits_MatchKubebuilderTags pins the +kubebuilder:default tags on
// api/v1alpha1.JSLimits to DefaultLimits(). Without this guard the two can
// drift silently — the CRD would advertise one default while the engine
// applies another, and users would only notice via mismatched behavior.
func TestDefaultLimits_MatchKubebuilderTags(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// repo/internal/jsengine/this_test.go → repo/api/v1alpha1/js_shared.go
	repoRoot := strings.TrimSuffix(thisFile, "/internal/jsengine/defaults_consistency_test.go")
	src := repoRoot + "/api/v1alpha1/js_shared.go"

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	got := map[string]int32{}
	defaultRE := regexp.MustCompile(`\+kubebuilder:default=(\d+)`)
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "JSLimits" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, field := range st.Fields.List {
			if field.Doc == nil || len(field.Names) == 0 {
				continue
			}
			for _, c := range field.Doc.List {
				m := defaultRE.FindStringSubmatch(c.Text)
				if m == nil {
					continue
				}
				v, err := strconv.Atoi(m[1])
				if err != nil {
					t.Fatalf("parse default %q: %v", m[1], err)
				}
				got[field.Names[0].Name] = int32(v)
			}
		}
		return false
	})

	want := DefaultLimits()
	if got["MemoryMB"] != want.MemoryMB {
		t.Errorf("JSLimits.MemoryMB +kubebuilder:default=%d, DefaultLimits().MemoryMB=%d — keep them in sync",
			got["MemoryMB"], want.MemoryMB)
	}
	if got["TimeoutSeconds"] != want.TimeoutSeconds {
		t.Errorf("JSLimits.TimeoutSeconds +kubebuilder:default=%d, DefaultLimits().TimeoutSeconds=%d — keep them in sync",
			got["TimeoutSeconds"], want.TimeoutSeconds)
	}
}
