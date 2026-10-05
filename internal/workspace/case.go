package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Case is one test: a request or an event, the cluster before, and what must
// hold after. One case per file in tests/.
type Case struct {
	Name    string           `json:"name"`
	Cluster []map[string]any `json:"cluster,omitempty"`
	Request *RequestSpec     `json:"request,omitempty"`
	Event   *EventSpec       `json:"event,omitempty"`
	Expect  Expect           `json:"expect"`
}

// Expect is what a case demands. Text fields are exact, or a regular
// expression between slashes.
type Expect struct {
	// Allowed defaults to true unless Error is set.
	Allowed  *bool            `json:"allowed,omitempty"`
	Message  string           `json:"message,omitempty"`
	Warnings []string         `json:"warnings,omitempty"`
	Object   map[string]any   `json:"object,omitempty"`
	Cluster  []map[string]any `json:"cluster,omitempty"`
	// Error expects the call to fail; plain text is a substring of the error.
	Error string `json:"error,omitempty"`
}

// LoadCase reads a case file.
func LoadCase(path string) (*Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Case{}
	if err := yaml.UnmarshalStrict(raw, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Name == "" {
		c.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return c, nil
}

// RunCase runs c against m and returns what differs from its expectations;
// an empty list means the case passed. The error is for a case that could not
// run at all.
func (m *Manifest) RunCase(ctx context.Context, c *Case) ([]string, error) {
	out, err := m.Run(ctx, Input{Request: c.Request, Event: c.Event, Cluster: c.Cluster})
	if err != nil {
		return nil, err
	}
	return c.Check(out), nil
}

// Check compares an output with the expectations.
// workspace.R4
// workspace.R5
// workspace.R6
func (c *Case) Check(out *Output) []string {
	e := c.Expect
	var diffs []string
	if e.Error != "" {
		if out.Error == "" {
			return []string{fmt.Sprintf("expected an error matching %s, but the call succeeded", e.Error)}
		}
		if !matchText(e.Error, out.Error, true) {
			diffs = append(diffs, fmt.Sprintf("error: want %s, got %q", e.Error, out.Error))
		}
		return diffs
	}
	if out.Error != "" {
		return []string{"unexpected error: " + out.Error}
	}
	if out.AdmissionResult != nil {
		want := e.Allowed == nil || *e.Allowed
		if out.Allowed != want {
			diffs = append(diffs, fmt.Sprintf("allowed: want %t, got %t", want, out.Allowed))
		}
		if e.Message != "" && !matchText(e.Message, out.Message, false) {
			diffs = append(diffs, fmt.Sprintf("message: want %s, got %q", e.Message, out.Message))
		}
		if e.Warnings != nil {
			diffs = append(diffs, checkWarnings(e.Warnings, out.Warnings)...)
		}
	}
	if e.Object != nil {
		diffs = append(diffs, subset("object", e.Object, normalize(out.PatchedObject))...)
	}
	for _, want := range e.Cluster {
		id := objectID(want)
		var got map[string]any
		for _, o := range out.Cluster {
			if objectID(o) == id {
				got = o
			}
		}
		if got == nil {
			diffs = append(diffs, fmt.Sprintf("cluster: %s is missing", id))
			continue
		}
		diffs = append(diffs, subset("cluster "+id, want, normalize(got))...)
	}
	return diffs
}

func checkWarnings(want, got []string) []string {
	var diffs []string
	if len(want) != len(got) {
		diffs = append(diffs, fmt.Sprintf("warnings: want %d, got %d %q", len(want), len(got), got))
	}
	for i := range want {
		if i < len(got) && !matchText(want[i], got[i], false) {
			diffs = append(diffs, fmt.Sprintf("warnings[%d]: want %s, got %q", i, want[i], got[i]))
		}
	}
	return diffs
}

// matchText: "/re/" is a regular expression (anywhere in got), anything else
// equals got, or is contained in it when contains is set.
func matchText(want, got string, contains bool) bool {
	if len(want) >= 2 && strings.HasPrefix(want, "/") && strings.HasSuffix(want, "/") {
		re, err := regexp.Compile(want[1 : len(want)-1])
		return err == nil && re.MatchString(got)
	}
	if contains {
		return strings.Contains(got, want)
	}
	return want == got
}

// normalize round-trips v through JSON, so numbers compare as the script and
// the case file would both see them.
func normalize(v map[string]any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}

// subset reports where got differs from want: every field of want must equal
// the field of got; maps compare recursively, lists element by element, extra
// fields of got are ignored.
// workspace.R4
func subset(path string, want, got any) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want an object, got %s", path, show(got))}
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var diffs []string
		for _, k := range keys {
			gv, ok := g[k]
			if !ok {
				diffs = append(diffs, fmt.Sprintf("%s.%s: want %s, missing", path, k, show(w[k])))
				continue
			}
			diffs = append(diffs, subset(path+"."+k, w[k], gv)...)
		}
		return diffs
	case []any:
		g, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want a list, got %s", path, show(got))}
		}
		var diffs []string
		for i := range w {
			if i >= len(g) {
				diffs = append(diffs, fmt.Sprintf("%s[%d]: want %s, missing", path, i, show(w[i])))
				continue
			}
			diffs = append(diffs, subset(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return diffs
	}
	if show(want) != show(got) {
		return []string{fmt.Sprintf("%s: want %s, got %s", path, show(want), show(got))}
	}
	return nil
}

func show(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}

// RunTests runs every case under dirs (default ".") and prints one line per
// case, a diff per failure and a summary. It returns the number of cases that
// failed.
// workspace.R7
func RunTests(ctx context.Context, w io.Writer, dirs []string) (int, error) {
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	workspaces, err := findWorkspaces(dirs)
	if err != nil {
		return 0, err
	}
	passed, failed := 0, 0
	for _, dir := range workspaces {
		files, _ := filepath.Glob(filepath.Join(dir, "tests", "*.yaml"))
		sort.Strings(files)
		if len(files) == 0 {
			continue
		}
		m, err := Load(dir)
		if err != nil {
			_, _ = fmt.Fprintf(w, "FAIL %s (load)\n     %v\n", dir, err)
			failed++
			continue
		}
		for _, f := range files {
			c, err := LoadCase(f)
			if err != nil {
				_, _ = fmt.Fprintf(w, "FAIL %s %s\n     %v\n", dir, filepath.Base(f), err)
				failed++
				continue
			}
			diffs, err := m.RunCase(ctx, c)
			if err != nil {
				diffs = []string{err.Error()}
			}
			if len(diffs) == 0 {
				_, _ = fmt.Fprintf(w, "ok   %s %s\n", dir, c.Name)
				passed++
				continue
			}
			_, _ = fmt.Fprintf(w, "FAIL %s %s\n", dir, c.Name)
			for _, d := range diffs {
				_, _ = fmt.Fprintf(w, "     %s\n", d)
			}
			failed++
		}
	}
	if passed+failed == 0 {
		_, _ = fmt.Fprintln(w, "no test cases found")
		return 0, nil
	}
	_, _ = fmt.Fprintf(w, "%d passed, %d failed\n", passed, failed)
	return failed, nil
}

// findWorkspaces lists the directories under dirs that hold a policy.yaml or
// hook.yaml.
func findWorkspaces(dirs []string) ([]string, error) {
	var found []string
	for _, root := range dirs {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if n := d.Name(); p != root && (n == ".git" || n == "node_modules") {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() == PolicyFile || d.Name() == HookFile {
				found = append(found, filepath.Dir(p))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(found)
	out := found[:0]
	for i, d := range found {
		if i == 0 || d != found[i-1] {
			out = append(out, d)
		}
	}
	return out, nil
}
