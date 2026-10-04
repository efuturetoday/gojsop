package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsrun"
)

func parse(t *testing.T, args ...string) (runFlags, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return parseFlagSet(fs, args)
}

// The backoff flags are what both reconcilers get in cmd/main.go (Backoff:
// f.backoff()); the reconciler tests show that it reaches the registry.
//
// js-registry.R17
func TestFlags_BuildBackoffReachesReconcilers(t *testing.T) {
	f, err := parse(t, "--build-backoff-base=2s", "--build-backoff-max=1m")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.backoff(), (jsrun.Backoff{Base: 2 * time.Second, Max: time.Minute}); got != want {
		t.Fatalf("backoff = %+v, want %+v", got, want)
	}

	def, err := parse(t)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := def.backoff(), (jsrun.Backoff{Base: time.Second, Max: 5 * time.Minute}); got != want {
		t.Fatalf("default backoff = %+v, want %+v", got, want)
	}
}

// js-registry.R17
func TestFlags_BuildBackoffRejectsNonsense(t *testing.T) {
	for _, args := range [][]string{
		{"--build-backoff-base=0s"},
		{"--build-backoff-base=10s", "--build-backoff-max=1s"},
	} {
		if _, err := parse(t, args...); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}

// js-execution.R12
func TestFlags_EngineCacheDir_DefaultsToInMemory(t *testing.T) {
	def, err := parse(t)
	if err != nil || def.engineCacheDir != "" {
		t.Fatalf("default engineCacheDir = %q, %v; want empty (in-memory cache)", def.engineCacheDir, err)
	}
	f, err := parse(t, "--engine-cache-dir=/var/cache/engine")
	if err != nil || f.engineCacheDir != "/var/cache/engine" {
		t.Fatalf("engineCacheDir = %q, %v", f.engineCacheDir, err)
	}
}

// The memory limit of the manager container holds the default number of
// concurrent calls at the default memory limit, the snapshot budget and the
// base of the process (the sizing formula of js-registry.R20).
//
// js-registry.R20
func TestManagerManifest_MemoryFitsDefaultCalls(t *testing.T) {
	const baseMiB, snapshotsMiB = 128, 128
	f, err := os.Open(filepath.Join("..", "config", "manager", "manager.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewYAMLOrJSONDecoder(f, 4096)
	var limit *resource.Quantity
	for {
		var d appsv1.Deployment
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if d.Kind != "Deployment" {
			continue
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name == "manager" {
				q := c.Resources.Limits[corev1.ResourceMemory]
				limit = &q
			}
		}
	}
	if limit == nil || limit.IsZero() {
		t.Fatal("no memory limit on the manager container")
	}
	needMiB := int64(jsregistry.DefaultMaxConcurrentCalls)*int64(jsrun.DefaultLimits().MemoryMB) + snapshotsMiB + baseMiB
	if got := limit.Value() >> 20; got < needMiB {
		t.Fatalf("manager memory limit %s (%d Mi) < %d calls x %d MB + %d Mi snapshots + %d Mi base = %d Mi",
			limit, got, jsregistry.DefaultMaxConcurrentCalls, jsrun.DefaultLimits().MemoryMB,
			snapshotsMiB, baseMiB, needMiB)
	}
}

// js-registry.R20
func TestFlags_MaxConcurrentCalls_DefaultsAndRejectsNonsense(t *testing.T) {
	def, err := parse(t)
	if err != nil || def.maxConcurrentCalls != jsregistry.DefaultMaxConcurrentCalls {
		t.Fatalf("default maxConcurrentCalls = %d, %v; want %d",
			def.maxConcurrentCalls, err, jsregistry.DefaultMaxConcurrentCalls)
	}

	f, err := parse(t, "--max-concurrent-calls=4")
	if err != nil || f.maxConcurrentCalls != 4 {
		t.Fatalf("maxConcurrentCalls = %d, %v", f.maxConcurrentCalls, err)
	}

	for _, args := range [][]string{
		{"--max-concurrent-calls=0"},
		{"--max-concurrent-calls=-1"},
	} {
		if _, err := parse(t, args...); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}

// jsadmission.R15
func TestFlags_AdmissionExclude_DefaultsToSystemNamespaces(t *testing.T) {
	def, err := parse(t)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gojsop-system", "kube-system", "cert-manager"}
	if got := def.excludedNamespaces("gojsop-system"); !slices.Equal(got, want) {
		t.Fatalf("default: got %v, want %v", got, want)
	}

	f, err := parse(t, "--admission-exclude-namespaces= kube-system ,,gojsop-system,infra")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"gojsop-system", "kube-system", "infra"}
	if got := f.excludedNamespaces("gojsop-system"); !slices.Equal(got, want) {
		t.Fatalf("custom: got %v, want %v", got, want)
	}

	none, err := parse(t, "--admission-exclude-namespaces=")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"gojsop-system"}
	if got := none.excludedNamespaces("gojsop-system"); !slices.Equal(got, want) {
		t.Fatalf("empty: got %v, want %v; the own namespace is always excluded", got, want)
	}
}
