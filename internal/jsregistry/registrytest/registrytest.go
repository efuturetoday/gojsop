// Package registrytest holds helpers that let a test build a VM in a
// jsregistry.Registry synchronously. Production code never waits for a build
// (js-registry.R15); a test that needs the VM now does.
package registrytest

import (
	"context"
	"time"

	"github.com/efuturetoday/gojsop/internal/jsregistry"
	"github.com/efuturetoday/gojsop/internal/jsrun"
)

// GetOrLoad calls Registry.Ensure for key until the key is Ready or Broken
// and returns the prepared script or the build error. The boolean reports
// whether a different script is installed than before the call. ctx bounds
// the wait only.
func GetOrLoad(reg *jsregistry.Registry, ctx context.Context, key jsrun.Key, opts jsrun.Spec) (*jsregistry.Prepared, bool, error) { //nolint:revive // mirrors the removed Registry.GetOrLoad
	before, _ := reg.Get(key)
	for {
		st := reg.Ensure(key, opts)
		switch st.Phase {
		case jsrun.PhaseReady:
			p, _ := reg.Get(key)
			return p, p != before, nil
		case jsrun.PhaseFailed:
			return nil, false, st.Err
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}
