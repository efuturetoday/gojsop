package dispatcher_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fastschema/qjs"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jshook"
	"github.com/o-haase/gojsop/internal/jshook/dispatcher"
	"github.com/o-haase/gojsop/internal/jsregistry"
)

var cmGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

const ns = "default"

// fixedMapper resolves every kind to ConfigMaps.
type fixedMapper struct{}

func (fixedMapper) RESTMapping(schema.GroupKind, ...string) (*dispatcher.RESTMapping, error) {
	return &dispatcher.RESTMapping{Resource: cmGVR}, nil
}

type emitted struct{ Type, Reason, Message string }

type env struct {
	t       *testing.T
	reg     *jsregistry.Registry
	d       *dispatcher.Dispatcher
	dyn     *fake.FakeDynamicClient
	key     types.NamespacedName
	watches chan struct{}

	mu     sync.Mutex
	events []emitted

	entered atomic.Int32 // handle() calls started (JS reached enter())
	ooms    atomic.Int32 // how many times oomNow() still answers true
}

// newEnv builds a hook VM from src. The JS sees the host functions enter()
// (counts started calls) and oomNow() (true while armed).
func newEnv(t *testing.T, src string, lim jsengine.Limits, objs ...runtime.Object) *env {
	t.Helper()
	e := &env{
		t:       t,
		reg:     jsregistry.NewRegistry(),
		key:     types.NamespacedName{Name: "hook"},
		watches: make(chan struct{}, 16),
	}
	e.dyn = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cmGVR: "ConfigMapList"}, objs...)
	// Signal when a watch is established, so the tests never create objects
	// into the fake's list-then-watch gap.
	e.dyn.PrependWatchReactor("*", func(a clienttesting.Action) (bool, watch.Interface, error) {
		w, err := e.dyn.Tracker().Watch(a.GetResource(), a.GetNamespace())
		e.watches <- struct{}{}
		return true, w, err
	})
	binder := jsengine.HostBinderFunc(func(c *qjs.Context) error {
		undef := func(t *qjs.This) (*qjs.Value, error) { return t.Context().NewUndefined(), nil }
		c.Global().SetPropertyStr("enter", c.Function(func(t *qjs.This) (*qjs.Value, error) {
			e.entered.Add(1)
			return undef(t)
		}))
		c.Global().SetPropertyStr("oomNow", c.Function(func(t *qjs.This) (*qjs.Value, error) {
			return t.Context().NewBool(e.ooms.Add(-1) >= 0), nil
		}))
		return nil
	})
	_, _, err := e.reg.GetOrLoad(context.Background(), e.key, jsregistry.BuildOptions{
		Source: []byte(src), SourceHash: "h", Limits: lim, Binder: binder,
	})
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	e.d = dispatcher.New(e.dyn, fixedMapper{}, e.reg)
	t.Cleanup(func() {
		e.d.Drop(e.key)
		e.reg.Drop(e.key)
	})
	return e
}

func (e *env) emit(typ, reason, msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, emitted{typ, reason, msg})
}

func (e *env) emitted() []emitted {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]emitted(nil), e.events...)
}

// subscribe starts the hook and waits until the informer watch is live.
func (e *env) subscribe(b jshook.KubernetesBinding) {
	e.t.Helper()
	if b.Name == "" {
		b.Name = "cms"
	}
	if b.APIVersion == "" {
		b.APIVersion, b.Kind = "v1", "ConfigMap"
	}
	cfg := &jshook.Config{Kubernetes: []jshook.KubernetesBinding{b}}
	if err := e.d.Subscribe(context.Background(), e.key, cfg, e.emit); err != nil {
		e.t.Fatalf("Subscribe: %v", err)
	}
	select {
	case <-e.watches:
	case <-time.After(5 * time.Second):
		e.t.Fatal("watch never established")
	}
}

type call = map[string]any

// calls returns the contexts the hook has recorded in globalThis.log. It goes
// through Registry.Call, so it waits for a running handle() to finish.
func (e *env) calls() []call {
	e.t.Helper()
	var out string
	_, _, err := e.reg.Call(context.Background(), e.key, func(ctx context.Context, vm *jsengine.VM) error {
		var err error
		out, err = vm.Eval(ctx, "log.js", `JSON.stringify(globalThis.log || [])`)
		return err
	})
	if err != nil {
		e.t.Fatalf("read log: %v", err)
	}
	var cs []call
	if err := json.Unmarshal([]byte(out), &cs); err != nil {
		e.t.Fatalf("decode log %q: %v", out, err)
	}
	return cs
}

func (e *env) waitCalls(n int) []call {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		cs := e.calls()
		if len(cs) >= n {
			return cs
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("want %d calls, got %d: %v", n, len(cs), cs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) waitEntered(above int32) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.entered.Load() <= above {
		if time.Now().After(deadline) {
			e.t.Fatal("handle never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func objName(c call) string {
	if o, ok := c["object"].(map[string]any); ok {
		md, _ := o["metadata"].(map[string]any)
		n, _ := md["name"].(string)
		return n
	}
	return ""
}

func objData(c call, key string) string {
	o, _ := c["object"].(map[string]any)
	d, _ := o["data"].(map[string]any)
	s, _ := d[key].(string)
	return s
}

func newCM(name, uid, v string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": uid},
		"data":     map[string]any{"v": v},
	}}
}

func (e *env) create(o *unstructured.Unstructured) {
	e.t.Helper()
	if _, err := e.dyn.Resource(cmGVR).Namespace(ns).Create(context.Background(), o, metav1.CreateOptions{}); err != nil {
		e.t.Fatalf("create: %v", err)
	}
}

func (e *env) update(o *unstructured.Unstructured) {
	e.t.Helper()
	if _, err := e.dyn.Resource(cmGVR).Namespace(ns).Update(context.Background(), o, metav1.UpdateOptions{}); err != nil {
		e.t.Fatalf("update: %v", err)
	}
}

func (e *env) remove(name string) {
	e.t.Helper()
	if err := e.dyn.Resource(cmGVR).Namespace(ns).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		e.t.Fatalf("delete: %v", err)
	}
}

func (e *env) waitEmitted(reason string, n int) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c := 0
		for _, ev := range e.emitted() {
			if ev.Reason == reason {
				c++
			}
		}
		if c >= n {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("want %d %s events, got %v", n, reason, e.emitted())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func boolp(b bool) *bool { return &b }

const recordSrc = `
globalThis.log = [];
function handle(c) { log.push(c[0]); }
`

// jshook.R3
// jshook.R4
func TestDispatcher_SynchronizationFirstThenDeltas(t *testing.T) {
	e := newEnv(t, recordSrc, jsengine.Limits{}, newCM("a", "uid-a", "1"))
	e.subscribe(jshook.KubernetesBinding{})
	e.create(newCM("b", "uid-b", "1"))

	cs := e.waitCalls(2)
	// R3: the snapshot is delivered first, the later change after it.
	if cs[0]["type"] != "Synchronization" {
		t.Fatalf("first context is %v, want Synchronization", cs[0])
	}
	objs, _ := cs[0]["objects"].([]any)
	if len(objs) != 1 {
		t.Fatalf("snapshot has %d objects, want 1: %v", len(objs), cs[0])
	}
	if cs[1]["type"] != "Event" || cs[1]["watchEvent"] != "Added" || objName(cs[1]) != "b" {
		t.Fatalf("second context is %v, want Added b", cs[1])
	}
	// R4: "a" is in the snapshot and must not come again as Added.
	time.Sleep(200 * time.Millisecond)
	for _, c := range e.calls()[1:] {
		if objName(c) == "a" {
			t.Fatalf("object a was delivered again after the snapshot: %v", c)
		}
	}
}

// jshook.R5
func TestDispatcher_SyncDisabled_ExistingObjectsArriveAsAdded(t *testing.T) {
	e := newEnv(t, recordSrc, jsengine.Limits{}, newCM("a", "uid-a", "1"))
	e.subscribe(jshook.KubernetesBinding{ExecuteHookOnSynchronization: boolp(false)})
	e.create(newCM("b", "uid-b", "1"))

	e.waitCalls(2)
	time.Sleep(200 * time.Millisecond)
	cs := e.calls()
	if len(cs) != 2 {
		t.Fatalf("want 2 calls, got %d: %v", len(cs), cs)
	}
	names := map[string]bool{}
	for _, c := range cs {
		if c["type"] != "Event" || c["watchEvent"] != "Added" {
			t.Fatalf("context %v, want Event/Added and no Synchronization", c)
		}
		names[objName(c)] = true
	}
	if !names["a"] || !names["b"] {
		t.Fatalf("want Added for a and b, got %v", names)
	}
}

// jshook.R6
func TestDispatcher_ExecuteHookOnEvent_ListedTypesOnly(t *testing.T) {
	e := newEnv(t, recordSrc, jsengine.Limits{})
	e.subscribe(jshook.KubernetesBinding{ExecuteHookOnEvent: []string{"Deleted"}})
	e.waitCalls(1) // Synchronization (empty)

	e.create(newCM("a", "uid-a", "1"))
	e.update(newCM("a", "uid-a", "2"))
	e.remove("a")

	e.waitCalls(2)
	time.Sleep(200 * time.Millisecond)
	cs := e.calls()
	if len(cs) != 2 || cs[1]["watchEvent"] != "Deleted" {
		t.Fatalf("want Synchronization then Deleted only, got %v", cs)
	}
}

// jshook.R6
func TestDispatcher_ExecuteHookOnEvent_EmptyMeansAll(t *testing.T) {
	e := newEnv(t, recordSrc, jsengine.Limits{})
	e.subscribe(jshook.KubernetesBinding{})
	e.waitCalls(1)

	e.create(newCM("a", "uid-a", "1"))
	e.waitCalls(2)
	e.update(newCM("a", "uid-a", "2"))
	e.waitCalls(3)
	e.remove("a")
	cs := e.waitCalls(4)
	got := []any{cs[1]["watchEvent"], cs[2]["watchEvent"], cs[3]["watchEvent"]}
	want := []any{"Added", "Modified", "Deleted"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events %v, want %v", got, want)
		}
	}
}

// jshook.R8
func TestDispatcher_BurstOfChangesFoldsIntoNewestObject(t *testing.T) {
	e := newEnv(t, recordSrc, jsengine.Limits{}, newCM("a", "uid-a", "0"))
	e.subscribe(jshook.KubernetesBinding{})
	e.waitCalls(1)

	// Hold the VM so the worker blocks inside its next call; later changes
	// pile up in the queue meanwhile.
	mi, _ := e.reg.Get(e.key)
	mi.CallMu.Lock()
	e.update(newCM("a", "uid-a", "1")) // goes in flight
	time.Sleep(200 * time.Millisecond)
	e.update(newCM("a", "uid-a", "2"))
	e.update(newCM("a", "uid-a", "3"))
	e.update(newCM("a", "uid-a", "4"))
	time.Sleep(300 * time.Millisecond)
	mi.CallMu.Unlock()

	e.waitCalls(3)
	time.Sleep(300 * time.Millisecond)
	cs := e.calls()
	if len(cs) != 3 {
		t.Fatalf("want Synchronization + in-flight + one folded call, got %d: %v", len(cs), cs)
	}
	if objData(cs[2], "v") != "4" {
		t.Fatalf("folded call carries v=%q, want newest 4", objData(cs[2], "v"))
	}
}

// jshook.R10
// jshook.R13
func TestDispatcher_ThrowingHandle_WarnsRetriesWithoutRestart(t *testing.T) {
	const src = `
globalThis.log = [];
globalThis.fails = 2;
function handle(c) {
  log.push(c[0]);
  if (globalThis.fails-- > 0) { throw new Error("boom"); }
}`
	e := newEnv(t, src, jsengine.Limits{})
	before, _ := e.reg.Get(e.key)
	e.subscribe(jshook.KubernetesBinding{})

	e.waitCalls(3) // two failures and the success
	e.waitEmitted(conditions.EventHandleFailed, 2)
	for _, ev := range e.emitted() {
		if ev.Reason == conditions.EventHandleFailed && ev.Type != corev1.EventTypeWarning {
			t.Fatalf("event %v is not a Warning", ev)
		}
		if ev.Reason == conditions.EventRestarted {
			t.Fatalf("a thrown error must not restart the VM: %v", e.emitted())
		}
	}
	after, _ := e.reg.Get(e.key)
	if after != before || len(after.History) != 0 {
		t.Fatalf("VM was replaced after a thrown error (history %v)", after.History)
	}
	// R13: success ends the retries.
	time.Sleep(300 * time.Millisecond)
	if n := len(e.calls()); n != 3 {
		t.Fatalf("handle ran %d times, want 3 (retries must stop after success)", n)
	}
}

// jshook.R12 (memory-limit half only; the panic half is blocked by DISP-12)
func TestDispatcher_MemoryLimit_RestartsAtOnceAndRetries(t *testing.T) {
	const src = `
globalThis.log = [];
function handle(c) {
  if (oomNow()) { var a = []; while (true) { a.push(new Array(100000).fill(1)); } }
  log.push(c[0]);
}`
	e := newEnv(t, src, jsengine.Limits{MemoryMB: 1})
	e.ooms.Store(1)
	first, _ := e.reg.Get(e.key)
	e.subscribe(jshook.KubernetesBinding{})

	e.waitEmitted(conditions.EventRestarted, 1)
	if ev := e.emitted()[0]; ev.Message != "restarted: "+string(jsregistry.ReasonMemoryLimit) {
		t.Fatalf("event %v, want restarted: memory-limit", ev)
	}
	cs := e.waitCalls(1)
	if cs[0]["type"] != "Synchronization" {
		t.Fatalf("retried context %v", cs[0])
	}
	if now, _ := e.reg.Get(e.key); now == first {
		t.Fatal("VM was not replaced")
	}
}

// jshook.R13
func TestDispatcher_RetryKeepsFresherStateOfSameObject(t *testing.T) {
	const src = `
globalThis.log = [];
function handle(c) {
  enter();
  log.push(c[0]);
  if (c[0].type === "Event" && !globalThis.failed) {
    globalThis.failed = true;
    var end = Date.now() + 400;
    while (Date.now() < end) {}
    throw new Error("boom");
  }
}`
	e := newEnv(t, src, jsengine.Limits{}, newCM("a", "uid-a", "0"))
	e.subscribe(jshook.KubernetesBinding{})
	e.waitCalls(1)
	before := e.entered.Load()

	e.update(newCM("a", "uid-a", "1")) // call fails after a delay
	e.waitEntered(before)
	e.update(newCM("a", "uid-a", "2")) // arrives while v=1 is failing

	e.waitCalls(3)
	time.Sleep(400 * time.Millisecond)
	cs := e.calls()
	if len(cs) != 3 {
		t.Fatalf("want 3 calls (sync, failed v=1, retry), got %d: %v", len(cs), cs)
	}
	if objData(cs[1], "v") != "1" || objData(cs[2], "v") != "2" {
		t.Fatalf("retry carries v=%q after failed v=%q, want the fresher 2", objData(cs[2], "v"), objData(cs[1], "v"))
	}
}

// jshook.R14
func TestDispatcher_Drop_NoMoreEvents(t *testing.T) {
	const src = `
globalThis.log = [];
function handle(c) { enter(); log.push(c[0]); }`
	e := newEnv(t, src, jsengine.Limits{})
	e.subscribe(jshook.KubernetesBinding{})
	e.waitCalls(1)

	e.d.Drop(e.key)
	n := e.entered.Load()
	e.create(newCM("a", "uid-a", "1"))
	e.update(newCM("a", "uid-a", "2"))
	time.Sleep(400 * time.Millisecond)
	if got := e.entered.Load(); got != n {
		t.Fatalf("handle ran %d more times after Drop", got-n)
	}
}
