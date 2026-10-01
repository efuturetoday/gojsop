package conditions

import (
	"errors"
	"fmt"
	"testing"

	"github.com/o-haase/gojsop/internal/jsrun"
)

// status-conditions.R3
// TestClassifyBuildError pins the typed-error → (reason, message) mapping
// the JSHook/JSAdmission reconcilers depend on. The whole point of typed
// sentinels is that this stays correct under message-string churn.
func TestClassifyBuildError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantReason string
		wantMsg    string
	}{
		{
			"missing export -> EntrypointMissing carrying name",
			&jsrun.MissingExportError{Name: "handle"},
			EventEntrypointMissing,
			"missing required export: handle()",
		},
		{
			"wrapped missing export still extracted via errors.As",
			fmt.Errorf("registry: post-build: %w", &jsrun.MissingExportError{Name: "validate"}),
			EventEntrypointMissing,
			"missing required export: validate()",
		},
		{
			"load module -> ModuleLoadFailed (static msg)",
			fmt.Errorf("%w: %v", jsrun.ErrLoadModule, errors.New("SyntaxError: at line 1")),
			EventModuleLoadFailed,
			"module load failed",
		},
		{
			"post-build (non-MissingExport) -> ConfigInvalid (static msg)",
			fmt.Errorf("%w: %v", jsrun.ErrPostBuild, errors.New("config() returned non-object")),
			EventConfigInvalid,
			"config() returned an error",
		},
		{
			"bind host -> BuildFailed bind host",
			fmt.Errorf("%w: %v", jsrun.ErrBindHost, errors.New("kube binder failed")),
			EventBuildFailed,
			"build failed: bind host",
		},
		{
			"new VM -> BuildFailed new vm",
			fmt.Errorf("%w: %v", jsrun.ErrNewVM, errors.New("engine init failed")),
			EventBuildFailed,
			"build failed: new vm",
		},
		{
			"unrecognized -> BuildFailed unknown (no error string leak)",
			errors.New("unexpected reflux from neptune"),
			EventBuildFailed,
			"build failed: unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotR, gotM := ClassifyBuildError(tc.err)
			if gotR != tc.wantReason {
				t.Errorf("reason: got %q want %q", gotR, tc.wantReason)
			}
			if gotM != tc.wantMsg {
				t.Errorf("message: got %q want %q", gotM, tc.wantMsg)
			}
		})
	}
}
