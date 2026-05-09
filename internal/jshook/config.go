package jshook

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/o-haase/gojsop/internal/jsengine"
)

// ReadConfig calls the module's exported `config()` if present and decodes
// the returned object as Config. Returns (nil, nil) when the module does
// not export config() — JSHook reconciles enforce non-nil at their layer
// (event subscriptions require config); JSAdmission policies don't call
// this path's result at all.
//
// ctx is the build / reconcile context — config() runs once per build, so
// this gets the reconcile deadline rather than the per-call timeout.
func ReadConfig(ctx context.Context, vm *jsengine.VM) (*Config, error) {
	if !vm.HasExport("config") {
		return nil, nil
	}
	raw, err := vm.CallExport(ctx, "config")
	if err != nil {
		return nil, fmt.Errorf("calling config(): %w", err)
	}
	if raw == "" {
		return nil, nil
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("config() returned non-JSON: %w (raw=%s)", err, raw)
	}
	return &cfg, nil
}
