package controller

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/o-haase/gojsop/internal/jsregistry"
)

// jshook.R2
func TestReadConfig_PostBuildRejectsMissingExports(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		missing string
	}{
		{"no config", `function handle(c) {}`, "config"},
		{"no handle", `function config() { return {}; }`, "handle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := jsregistry.NewRegistry()
			key := types.NamespacedName{Name: "h"}
			t.Cleanup(func() { reg.Drop(jsregistry.HookKey(key)) })

			_, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), jsregistry.BuildOptions{
				Source: []byte(tc.src), SourceHash: "x", PostBuild: readConfig,
			})
			var miss *jsregistry.MissingExportError
			if !errors.As(err, &miss) || miss.Name != tc.missing {
				t.Fatalf("GetOrLoad error = %v, want MissingExportError for %q", err, tc.missing)
			}
			if _, ok := reg.Get(jsregistry.HookKey(key)); ok {
				t.Fatal("a hook that failed to build must not be registered")
			}
		})
	}
}
