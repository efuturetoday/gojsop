// Command checkblocks keeps the blocks under .agents/blocks in sync with the
// Go code. It parses every Go file with go/parser (a syntax-tree parser, so
// renames and moves are caught, line drift is irrelevant) and fails when a
// block names a symbol or test that no longer exists, when a rule has no
// Gate line, or when a "// Block: <id> <rule>" anchor in the code points to a
// block or rule that does not exist.
//
// Only blocks with YAML front matter are checked, so blocks can migrate to
// the new format one at a time.
//
// Usage: go run ./hack/checkblocks
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const blocksDir = ".agents/blocks"

// skipDirs are directories that hold no project Go code.
var skipDirs = map[string]bool{"bin": true, "vendor": true, "testdata": true, "graft": true}

var (
	backtickRe = regexp.MustCompile("`([^`]+)`")
	symbolRe   = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[A-Za-z_][A-Za-z0-9_]*){1,2}$`)
	testRe     = regexp.MustCompile(`^(Test|Benchmark)[A-Za-z0-9_]*\*?$`)
	ruleRe     = regexp.MustCompile(`^- \*\*(R[0-9]+)\*\*`)
	anchorRe   = regexp.MustCompile(`Block: ([a-z0-9-]+) (R[0-9]+)`)
)

// index holds everything the blocks may reference.
type index struct {
	pkgs    map[string]bool // package names
	symbols map[string]bool // pkg.Name and pkg.Type.Member
	tests   map[string]bool // Test* and Benchmark* function names
	anchors []anchor
}

type anchor struct {
	pos   string
	block string
	rule  string
}

type block struct {
	path        string
	id          string
	entrypoints []string
	rules       map[string]bool
}

func main() {
	idx, err := indexGo(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "checkblocks:", err)
		os.Exit(2)
	}
	blocks, problems, err := checkBlocks(idx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "checkblocks:", err)
		os.Exit(2)
	}
	problems = append(problems, checkAnchors(idx, blocks)...)

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println(p)
		}
		fmt.Printf("checkblocks: %d problem(s)\n", len(problems))
		os.Exit(1)
	}
	fmt.Printf("checkblocks: %d block(s) in sync, %d anchor(s)\n", len(blocks), len(idx.anchors))
}

// indexGo parses all Go files below root and records declared symbols,
// test functions and block anchors in comments.
func indexGo(root string) (*index, error) {
	idx := &index{pkgs: map[string]bool{}, symbols: map[string]bool{}, tests: map[string]bool{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || skipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		pkg := strings.TrimSuffix(f.Name.Name, "_test")
		idx.pkgs[pkg] = true
		for _, decl := range f.Decls {
			indexDecl(idx, pkg, decl)
		}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				for _, m := range anchorRe.FindAllStringSubmatch(c.Text, -1) {
					idx.anchors = append(idx.anchors, anchor{pos: fset.Position(c.Pos()).String(), block: m[1], rule: m[2]})
				}
			}
		}
		return nil
	})
	return idx, err
}

func indexDecl(idx *index, pkg string, decl ast.Decl) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil || len(d.Recv.List) == 0 {
			idx.symbols[pkg+"."+d.Name.Name] = true
			if testRe.MatchString(d.Name.Name) {
				idx.tests[d.Name.Name] = true
			}
			return
		}
		if recv := recvName(d.Recv.List[0].Type); recv != "" {
			idx.symbols[pkg+"."+recv+"."+d.Name.Name] = true
		}
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				idx.symbols[pkg+"."+s.Name.Name] = true
				indexMembers(idx, pkg+"."+s.Name.Name, s.Type)
			case *ast.ValueSpec:
				for _, n := range s.Names {
					idx.symbols[pkg+"."+n.Name] = true
				}
			}
		}
	}
}

// indexMembers records struct fields and interface methods as Type.Member.
func indexMembers(idx *index, prefix string, expr ast.Expr) {
	var fields *ast.FieldList
	switch t := expr.(type) {
	case *ast.StructType:
		fields = t.Fields
	case *ast.InterfaceType:
		fields = t.Methods
	}
	if fields == nil {
		return
	}
	for _, f := range fields.List {
		for _, n := range f.Names {
			idx.symbols[prefix+"."+n.Name] = true
		}
	}
}

func recvName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func checkBlocks(idx *index) (map[string]*block, []string, error) {
	paths, err := filepath.Glob(filepath.Join(blocksDir, "*.md"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(paths)
	blocks := map[string]*block{}
	var problems []string
	for _, path := range paths {
		if filepath.Base(path) == "_template.md" {
			continue
		}
		b, p, err := checkBlock(idx, path)
		if err != nil {
			return nil, nil, err
		}
		if b != nil {
			blocks[b.id] = b
		}
		problems = append(problems, p...)
	}
	return blocks, problems, nil
}

// checkBlock validates one block. It returns nil for blocks without front
// matter (not yet migrated).
func checkBlock(idx *index, path string) (*block, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if len(lines) == 0 || lines[0] != "---" {
		return nil, nil, nil
	}

	b := &block{path: path, rules: map[string]bool{}}
	var problems []string
	report := func(line int, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s:%d: %s", path, line+1, fmt.Sprintf(format, args...)))
	}

	// Front matter.
	end := 1
	inEntrypoints := false
	for ; end < len(lines) && lines[end] != "---"; end++ {
		l := lines[end]
		switch {
		case strings.HasPrefix(l, "id:"):
			b.id = strings.TrimSpace(strings.TrimPrefix(l, "id:"))
		case strings.HasPrefix(l, "entrypoints:"):
			inEntrypoints = true
		case inEntrypoints && strings.HasPrefix(strings.TrimSpace(l), "- "):
			sym := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- "))
			b.entrypoints = append(b.entrypoints, sym)
			if !idx.symbols[sym] {
				report(end, "entrypoint %s not found in code", sym)
			}
		default:
			inEntrypoints = false
		}
	}
	if b.id == "" {
		report(0, "front matter has no id")
	}
	if want := strings.TrimSuffix(filepath.Base(path), ".md"); b.id != "" && b.id != want {
		report(0, "id %q does not match file name %q", b.id, want)
	}

	// Body: rules need a Gate line, backticked symbols and tests must exist.
	currentRule, currentLine, hasGate := "", 0, false
	closeRule := func() {
		if currentRule != "" && !hasGate {
			report(currentLine, "rule %s has no Gate line", currentRule)
		}
	}
	for i := end + 1; i < len(lines); i++ {
		l := lines[i]
		if m := ruleRe.FindStringSubmatch(l); m != nil {
			closeRule()
			currentRule, currentLine, hasGate = m[1], i, false
			b.rules[m[1]] = true
		} else if strings.HasPrefix(l, "#") {
			closeRule()
			currentRule = ""
		}
		if strings.Contains(l, "Gate:") {
			hasGate = true
		}
		missing := strings.Contains(l, "missing")
		for _, m := range backtickRe.FindAllStringSubmatch(l, -1) {
			ref := m[1]
			switch {
			case testRe.MatchString(ref):
				if !missing && !testExists(idx, ref) {
					report(i, "test %s not found in code", ref)
				}
			case symbolRe.MatchString(ref) && idx.pkgs[strings.SplitN(ref, ".", 2)[0]]:
				if !idx.symbols[ref] {
					report(i, "symbol %s not found in code", ref)
				}
			}
		}
	}
	closeRule()
	return b, problems, nil
}

// testExists accepts an exact name or a prefix written as "TestFoo_*".
func testExists(idx *index, ref string) bool {
	if prefix, ok := strings.CutSuffix(ref, "*"); ok {
		for t := range idx.tests {
			if strings.HasPrefix(t, prefix) {
				return true
			}
		}
		return false
	}
	return idx.tests[ref]
}

func checkAnchors(idx *index, blocks map[string]*block) []string {
	var problems []string
	for _, a := range idx.anchors {
		b, ok := blocks[a.block]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: anchor names unknown or unmigrated block %q", a.pos, a.block))
		case !b.rules[a.rule]:
			problems = append(problems,
				fmt.Sprintf("%s: anchor names rule %s, which block %q does not define", a.pos, a.rule, a.block))
		}
	}
	return problems
}
