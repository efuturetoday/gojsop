package jsrun_test

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	modPath    = "github.com/efuturetoday/gojsop/"
	enginePkg  = modPath + "internal/jsengine"
	oldQJSPkg  = "github.com/fastschema/qjs" // replaced by our own build (EXEC-8)
	wazeroPkg  = "github.com/tetratelabs/wazero"
	registry   = modPath + "internal/jsregistry"
	sourcePkg  = modPath + "internal/jssource"
	kubehost   = modPath + "internal/jsengine/kubehost"
	jslogPkg   = modPath + "internal/jslog"
	runnerPort = modPath + "internal/jsrun"
)

// imports maps every package of the module to its direct, non-test imports.
func imports(t *testing.T) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-json=ImportPath,Imports", "./...")
	cmd.Dir = "../.."
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	pkgs := map[string][]string{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p struct {
			ImportPath string
			Imports    []string
		}
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		pkgs[p.ImportPath] = p.Imports
	}
	return pkgs
}

func importsPkg(imps []string, prefix string) bool {
	return slices.ContainsFunc(imps, func(i string) bool {
		return i == prefix || strings.HasPrefix(i, prefix+"/")
	})
}

// callers are the packages that run scripts without owning the engine.
func isCaller(pkg string) bool {
	for _, root := range []string{"internal/jshook", "internal/jsadmission"} {
		if pkg == modPath+root || strings.HasPrefix(pkg, modPath+root+"/") {
			return true
		}
	}
	return pkg == modPath+"internal/jslifecycle" || pkg == modPath+"internal/conditions"
}

// js-execution.R2
// js-execution.R10
// js-registry.R1
// js-registry.R9
func TestImportBoundary_CallersUseOnlyRunnerPort(t *testing.T) {
	pkgs := imports(t)
	seen := 0
	for pkg, imps := range pkgs {
		if !isCaller(pkg) {
			continue
		}
		seen++
		for _, banned := range []string{enginePkg, wazeroPkg, registry} {
			// the host adapters under jsengine/ (kubehost) are not the engine.
			if slices.Contains(imps, banned) || (banned != enginePkg && importsPkg(imps, banned)) {
				t.Errorf("%s imports %s: reach script execution through jsrun.Runner", pkg, banned)
			}
		}
	}
	if seen < 6 {
		t.Fatalf("only %d caller packages found: the package filter is stale", seen)
	}
}

// js-execution.R1
// js-sources.R1
func TestImportBoundary_EngineStaysBehindRegistry(t *testing.T) {
	for pkg, imps := range imports(t) {
		inEngine := pkg == enginePkg || strings.HasPrefix(pkg, enginePkg+"/")
		if !inEngine && importsPkg(imps, wazeroPkg) {
			t.Errorf("%s imports wazero: only internal/jsengine/** may", pkg)
		}
		if importsPkg(imps, oldQJSPkg) {
			t.Errorf("%s imports fastschema/qjs: the engine is our own QuickJS-ng build", pkg)
		}
		// Host binders (kubehost, jslog) define the surface a script sees and
		// are part of the engine side; everyone else goes through jsrun.
		if slices.Contains(imps, enginePkg) && pkg != registry && pkg != kubehost && pkg != jslogPkg {
			t.Errorf("%s imports internal/jsengine: only jsregistry and the host binders may", pkg)
		}
		if pkg == registry && slices.Contains(imps, sourcePkg) {
			t.Errorf("jsregistry imports jssource: sources are loaded by controllers (js-registry.R3)")
		}
		if pkg == runnerPort && slices.ContainsFunc(imps, func(i string) bool { return strings.HasPrefix(i, modPath) }) {
			t.Errorf("jsrun imports %v: the port depends on no adapter", imps)
		}
	}
}

// js-execution.R10
// The data path (dispatcher, admission server, Handle wrappers) holds a
// jsrun.Runner and nothing of the lifecycle side: no Scripts, Spec, State or
// Phase. Only the controllers (packages named controller) and cmd see those.
func TestImportBoundary_DataPathUsesOnlyRunnerSide(t *testing.T) {
	lifecycle := map[string]bool{"Scripts": true, "Spec": true, "State": true, "PostBuildHook": true}
	roots := []string{"../jshook", "../jsadmission"}
	seen := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "controller" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			seen++
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "jsrun" &&
					(lifecycle[sel.Sel.Name] || strings.HasPrefix(sel.Sel.Name, "Phase")) {
					t.Errorf("%s uses jsrun.%s: the data path depends on jsrun.Runner only", path, sel.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if seen < 6 {
		t.Fatalf("only %d data path files found: the root list is stale", seen)
	}
}
