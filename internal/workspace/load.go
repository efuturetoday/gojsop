// Package workspace is the logic behind the gojsop CLI: it loads a hook or
// policy from a directory and runs calls of its script against a fake
// cluster, one at a time (Run) or for `gojsop serve` (Serve). It always runs the script in the engine
// the operator uses; only the dynamic client is a fake.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
	"github.com/efuturetoday/gojsop/internal/jsrun"
)

const (
	// PolicyFile and HookFile name the manifests a workspace directory holds.
	PolicyFile = "policy.yaml"
	HookFile   = "hook.yaml"
)

// Manifest is a loaded hook or policy with its script.
type Manifest struct {
	// Dir is the directory of the manifest.
	Dir string
	// Name is metadata.name, or the directory name when unset.
	Name string
	// Source is the script.
	Source []byte
	// Hook is set for a JSHook, Policy for a JSAdmission; never both.
	Hook   *corev1alpha1.JSHook
	Policy *corev1alpha1.JSAdmission
}

// Key is the registry key of the manifest.
func (m *Manifest) Key() jsrun.Key {
	n := types.NamespacedName{Name: m.Name}
	if m.Hook != nil {
		return jsrun.HookKey(n)
	}
	return jsrun.AdmissionKey(n)
}

// Mutating reports whether the manifest is a mutating policy.
func (m *Manifest) Mutating() bool {
	return m.Policy != nil && m.Policy.Spec.Type == "mutating"
}

// Load reads the manifest at path (a policy.yaml or hook.yaml file, or a
// directory holding one) and its script.
// workspace.R2
func Load(path string) (*Manifest, error) {
	return load(path, nil)
}

// LoadWithSource reads the manifest at path like Load, but takes the script
// from the caller: @gojsop/testing bundles a TypeScript or module script
// and hands over the result. A manifest with spec.source.inline is an error,
// because then two scripts compete.
// workspace.R11
func LoadWithSource(path string, source []byte) (*Manifest, error) {
	return load(path, source)
}

func load(path string, source []byte) (*Manifest, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		path, err = manifestIn(path)
		if err != nil {
			return nil, err
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	var tm struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(raw, &tm); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	kind := tm.Kind
	if kind == "" {
		switch filepath.Base(path) {
		case HookFile:
			kind = "JSHook"
		case PolicyFile:
			kind = "JSAdmission"
		}
	}
	m := &Manifest{Dir: dir}
	var inline string
	switch kind {
	case "JSHook":
		m.Hook = &corev1alpha1.JSHook{}
		if err := yaml.UnmarshalStrict(raw, m.Hook); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		m.Name, inline = m.Hook.Name, m.Hook.Spec.Source.Inline
	case "JSAdmission":
		m.Policy = &corev1alpha1.JSAdmission{}
		if err := yaml.UnmarshalStrict(raw, m.Policy); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		m.Name, inline = m.Policy.Name, m.Policy.Spec.Source.Inline
	default:
		return nil, fmt.Errorf("%s: kind must be JSHook or JSAdmission, got %q", path, kind)
	}
	if m.Name == "" {
		m.Name = filepath.Base(dir)
	}
	if source != nil {
		if inline != "" {
			return nil, fmt.Errorf("%s: has spec.source.inline and a script file next to it; keep one", path)
		}
		m.Source = source
		return m, nil
	}
	if inline != "" {
		m.Source = []byte(inline)
		return m, nil
	}
	js, err := filepath.Glob(filepath.Join(dir, "*.js"))
	if err != nil {
		return nil, err
	}
	sort.Strings(js)
	switch len(js) {
	case 0:
		return nil, fmt.Errorf("%s: no spec.source.inline and no .js file next to the manifest", path)
	case 1:
		if m.Source, err = os.ReadFile(js[0]); err != nil {
			return nil, err
		}
		return m, nil
	}
	return nil, fmt.Errorf("%s: several .js files next to the manifest (%d), keep one or use spec.source.inline", path, len(js))
}

// manifestIn finds the one manifest file of a directory.
func manifestIn(dir string) (string, error) {
	var found []string
	for _, n := range []string{PolicyFile, HookFile} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			found = append(found, filepath.Join(dir, n))
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%s: neither %s nor %s", dir, PolicyFile, HookFile)
	case 1:
		return found[0], nil
	}
	return "", errors.New(dir + ": both policy.yaml and hook.yaml, keep one")
}
