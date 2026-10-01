// Package registrytest holds helpers that let a test build a VM in a
// jsregistry.Registry synchronously. Production code never waits for a build
// (js-registry.R15); a test that needs the VM now does.
package registrytest

import (
	"context"
	"time"

	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// GetOrLoad calls Registry.Ensure for key until the key is Ready or Broken
// and returns the VM or the build error. The boolean reports whether a
// different VM is installed than before the call. ctx bounds the wait only.
func GetOrLoad(reg *jsregistry.Registry, ctx context.Context, key jsrun.Key, opts jsrun.Options) (*jsregistry.ManagedVM, bool, error) { //nolint:revive // mirrors the removed Registry.GetOrLoad
	before, _ := reg.Get(key)
	for {
		st := reg.Ensure(key, opts)
		switch st.Kind {
		case jsrun.StateReady:
			mi, _ := reg.Get(key)
			return mi, mi != before, nil
		case jsrun.StateBroken:
			return nil, false, st.Err
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Restart calls Registry.Restart and waits until the rebuilt VM is Ready
// (or the build failed), like the synchronous restart the registry had before
// builds went to the background.
func Restart(reg *jsregistry.Registry, key jsrun.Key, reason jsrun.RestartReason) (*jsregistry.ManagedVM, error) {
	before, ok := reg.Get(key)
	if !ok {
		return nil, reg.Restart(key, reason) // the error for the unknown key
	}
	if err := reg.Restart(key, reason); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Minute)
	for {
		st := reg.Ensure(key, before.Opts)
		mi, _ := reg.Get(key)
		switch {
		case st.Kind == jsrun.StateReady && mi != before:
			return mi, nil
		case st.Kind == jsrun.StateBroken:
			return nil, st.Err
		case time.Now().After(deadline):
			return nil, context.DeadlineExceeded
		}
		time.Sleep(2 * time.Millisecond)
	}
}
