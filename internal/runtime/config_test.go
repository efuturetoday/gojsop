package runtime

import "testing"

func TestLoadConfig_FromHookSource(t *testing.T) {
	const src = `
		function config() {
		  return {
		    configVersion: "v1",
		    onStartup: 10,
		    kubernetes: [{
		      name: "watch-pods",
		      apiVersion: "v1",
		      kind: "Pod",
		      executeHookOnEvent: ["Added", "Modified"],
		      namespace: { nameSelector: { matchNames: ["default"] } },
		    }],
		  };
		}
	`
	inst, err := New(Resources{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)

	if err := inst.LoadModule("hook.js", src); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	cfg, err := inst.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ConfigVersion != "v1" {
		t.Errorf("ConfigVersion: got %q", cfg.ConfigVersion)
	}
	if cfg.OnStartup != 10 {
		t.Errorf("OnStartup: got %d", cfg.OnStartup)
	}
	if len(cfg.Kubernetes) != 1 {
		t.Fatalf("Kubernetes: got %d bindings", len(cfg.Kubernetes))
	}
	b := cfg.Kubernetes[0]
	if b.Kind != "Pod" || b.APIVersion != "v1" {
		t.Errorf("binding gvk: %+v", b)
	}
	if got := b.Namespace.NameSelector.MatchNames; len(got) != 1 || got[0] != "default" {
		t.Errorf("namespace selector: %+v", got)
	}
}

func TestLoadConfig_MissingFunction(t *testing.T) {
	inst, err := New(Resources{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)

	if err := inst.LoadModule("noconfig.js", "var x = 1;"); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	if _, err := inst.LoadConfig(); err == nil {
		t.Fatal("expected error when hook has no config()")
	}
}
