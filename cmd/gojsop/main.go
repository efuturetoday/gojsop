// Command gojsop runs and tests hooks and policies on the maintainer's
// machine, in the engine the operator uses, against a cluster in memory.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"sigs.k8s.io/yaml"

	"github.com/efuturetoday/gojsop/internal/workspace"
)

const usage = `usage:
  gojsop run <manifest> (--request <file> | --event <file>) [--cluster <file>] [--trace]
  gojsop test [dir ...]
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// execute runs one command line and returns the exit code: 0 on success, 1
// when a script failed or a case did, 2 on bad usage.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return runCmd(ctx, args[1:], stdout, stderr)
	case "test":
		return testCmd(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "gojsop: unknown command %q\n%s", args[0], usage)
	return 2
}

func testCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	failed, err := workspace.RunTests(ctx, stdout, args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "gojsop:", err)
		return 2
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func runCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	request := fs.String("request", "", "admission request file (policy)")
	event := fs.String("event", "", "hook event file (hook)")
	clusterFile := fs.String("cluster", "", "file with the objects of the fake cluster")
	trace := fs.Bool("trace", false, "print every kube.* call to stderr")
	// The manifest comes first, the flags after it; flag stops at the first
	// non-flag, so parse twice.
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	manifest := rest[0]
	if err := fs.Parse(rest[1:]); err != nil || fs.NArg() > 0 {
		return 2
	}
	if (*request == "") == (*event == "") {
		_, _ = fmt.Fprintln(stderr, "gojsop: give exactly one of --request and --event")
		return 2
	}

	fail := func(err error) int {
		_, _ = fmt.Fprintln(stderr, "gojsop:", err)
		return 2
	}
	m, err := workspace.Load(manifest)
	if err != nil {
		return fail(err)
	}
	in := workspace.Input{}
	if *trace {
		in.Trace = stderr
	}
	if *request != "" {
		in.Request = &workspace.RequestSpec{}
		err = readStrict(*request, in.Request)
	} else {
		in.Event = &workspace.EventSpec{}
		err = readStrict(*event, in.Event)
	}
	if err != nil {
		return fail(err)
	}
	if *clusterFile != "" {
		if in.Cluster, err = readCluster(*clusterFile); err != nil {
			return fail(err)
		}
	}
	out, err := m.Run(ctx, in)
	if err != nil {
		return fail(err)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fail(err)
	}
	if out.Error != "" {
		return 1
	}
	return 0
}

func readStrict(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.UnmarshalStrict(raw, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// readCluster reads a list of objects, or a document with `items`.
func readCluster(path string) ([]map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []map[string]any
	if err := yaml.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var doc struct {
		Items []map[string]any `json:"items"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: want a list of objects: %w", path, err)
	}
	return doc.Items, nil
}
