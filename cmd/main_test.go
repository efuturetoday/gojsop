package main

import (
	"flag"
	"io"
	"testing"
	"time"

	"github.com/o-haase/gojsop/internal/jsregistry"
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
	if got, want := f.backoff(), (jsregistry.Backoff{Base: 2 * time.Second, Max: time.Minute}); got != want {
		t.Fatalf("backoff = %+v, want %+v", got, want)
	}

	def, err := parse(t)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := def.backoff(), (jsregistry.Backoff{Base: time.Second, Max: 5 * time.Minute}); got != want {
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
