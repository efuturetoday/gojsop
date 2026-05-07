package runtime

import "testing"

func TestEval_1Plus1(t *testing.T) {
	inst, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)

	got, err := inst.Eval("smoke.js", "1 + 1")
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got != "2" {
		t.Fatalf("expected 2, got %q", got)
	}
}

func TestEval_PersistentState(t *testing.T) {
	// Top-level state must survive between Eval calls — this is the
	// persistent-instance contract we promised users.
	inst, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)

	if _, err := inst.Eval("setup.js", "globalThis.counter = 0;"); err != nil {
		t.Fatalf("setup eval: %v", err)
	}
	for i := 1; i <= 3; i++ {
		got, err := inst.Eval("inc.js", "globalThis.counter++; globalThis.counter")
		if err != nil {
			t.Fatalf("inc eval: %v", err)
		}
		want := []string{"", "1", "2", "3"}[i]
		if got != want {
			t.Fatalf("iter %d: want %s, got %q", i, want, got)
		}
	}
}
