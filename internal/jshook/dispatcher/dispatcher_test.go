package dispatcher_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jshook/dispatcher"
	"github.com/o-haase/gojsop/internal/jslog"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsregistry/registrytest"
	"github.com/o-haase/gojsop/internal/jsrun"
)

var cmGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

var cmRule = corev1alpha1.ResourceRule{
	APIGroups:   []string{""},
	APIVersions: []string{"v1"},
	Resources:   []string{"configmaps"},
}

const ns = "default"

// fixedMapper serves ConfigMaps and nothing else.
type fixedMapper struct{}

func (fixedMapper) KindFor(gvr schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	if gvr != cmGVR {
		return schema.GroupVersionKind{}, fmt.Errorf("no match for %s", gvr)
	}
	return schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, nil
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
	log    []call // contexts handed to record()

	entered atomic.Int32 // handle() calls started (JS reached enter())
	ooms    atomic.Int32 // how many times oomNow() still answers true
	crashes atomic.Int32 // how many times crashNow() still panics in the host
	fails   atomic.Int32 // how many times failNow() still answers true

	// hold, while set, blocks every build of the hook's VM until it is closed.
	hold atomic.Pointer[chan struct{}]
	// gate, while set, blocks every waitGate() until it is closed.
	gate atomic.Pointer[chan struct{}]

	spec jsrun.Spec
}

// newEnv builds a hook VM from src. Every call starts from the snapshot, so the
// JS keeps nothing between calls; the host functions keep the test state:
// enter() counts started calls, record(x) logs x, oomNow(), crashNow() and
// failNow() misbehave while armed, waitGate() blocks while a gate is set.
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
	hooks := jsengine.HostBinderFunc(func(h *jsengine.Host) error {
		h.Func("enter", func(context.Context, json.RawMessage) (any, error) {
			e.entered.Add(1)
			return nil, nil
		})
		h.Func("crashNow", func(context.Context, json.RawMessage) (any, error) {
			if e.crashes.Add(-1) >= 0 {
				panic("host function bug")
			}
			return false, nil
		})
		h.Func("oomNow", func(context.Context, json.RawMessage) (any, error) {
			return e.ooms.Add(-1) >= 0, nil
		})
		h.Func("failNow", func(context.Context, json.RawMessage) (any, error) {
			return e.fails.Add(-1) >= 0, nil
		})
		h.Func("record", func(_ context.Context, arg json.RawMessage) (any, error) {
			var c call
			if err := json.Unmarshal(arg, &c); err != nil {
				return nil, err
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			e.log = append(e.log, c)
			return nil, nil
		})
		h.Func("waitGate", func(ctx context.Context, _ json.RawMessage) (any, error) {
			if gate := e.gate.Load(); gate != nil {
				select {
				case <-*gate:
				case <-ctx.Done():
				}
			}
			return nil, nil
		})
		return nil
	})
	// The dispatcher's own surface plus the console every script gets.
	binder := jsengine.Binders(hooks, jslog.Binder{})
	e.spec = jsrun.Spec{
		Source: []byte(src), SourceHash: "h", Limits: lim, Host: binder,
		PostBuild: func(ctx context.Context, _ jsrun.Script) error {
			if gate := e.hold.Load(); gate != nil {
				select {
				case <-*gate:
				case <-ctx.Done():
				}
			}
			return nil
		},
	}
	_, _, err := registrytest.GetOrLoad(e.reg, context.Background(), jsrun.HookKey(e.key), e.spec)
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	e.d = dispatcher.New(e.dyn, fixedMapper{}, e.reg)
	t.Cleanup(func() {
		e.d.Drop(e.key)
		e.reg.Drop(jsrun.HookKey(e.key))
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
func (e *env) subscribe(b corev1alpha1.HookBinding) {
	e.t.Helper()
	if b.Name == "" {
		b.Name = "cms"
	}
	if len(b.Resources) == 0 {
		b.ResourceRule = cmRule
	}
	if err := e.d.Subscribe(context.Background(), e.key, []corev1alpha1.HookBinding{b}, nil, e.emit); err != nil {
		e.t.Fatalf("Subscribe: %v", err)
	}
	select {
	case <-e.watches:
	case <-time.After(5 * time.Second):
		e.t.Fatal("watch never established")
	}
}

type call = map[string]any

// calls returns the contexts the hook has handed to record() so far.
func (e *env) calls() []call {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]call(nil), e.log...)
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

// dataV returns data.v of the object a call received.
func dataV(c call) string {
	o, _ := c["object"].(map[string]any)
	d, _ := o["data"].(map[string]any)
	s, _ := d["v"].(string)
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

const (
	bcSynchronization = "Synchronization"
	bcEvent           = "Event"
)

func boolp(b bool) *bool { return &b }

const recordSrc = `
function handle(c) { record(c[0]); }
`

// jshook.R3
// jshook.R4
func TestDispatcher_SynchronizationFirstThenDeltas(t *testing.T) {
	e := newEnv(t, recordSrc, jsengine.Limits{}, newCM("a", "uid-a", "1"))
	e.subscribe(corev1alpha1.HookBinding{})
	e.create(newCM("b", "uid-b", "1"))

	cs := e.waitCalls(2)
	// R3: the snapshot is delivered first, the later change after it.
	if cs[0]["type"] != bcSynchronization {
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
	e.subscribe(corev1alpha1.HookBinding{Synchronization: boolp(false)})
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
	e.subscribe(corev1alpha1.HookBinding{Events: []corev1alpha1.HookEvent{corev1alpha1.HookEventDeleted}})
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
	e.subscribe(corev1alpha1.HookBinding{})
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
	const src = `
function handle(c) { waitGate(); record(c[0]); }`
	e := newEnv(t, src, jsengine.Limits{}, newCM("a", "uid-a", "0"))
	e.subscribe(corev1alpha1.HookBinding{})
	e.waitCalls(1)

	// Close the gate so the worker blocks inside its next call; later changes
	// pile up in the queue meanwhile.
	gate := make(chan struct{})
	e.gate.Store(&gate)
	e.update(newCM("a", "uid-a", "1")) // goes in flight
	time.Sleep(200 * time.Millisecond)
	e.update(newCM("a", "uid-a", "2"))
	e.update(newCM("a", "uid-a", "3"))
	e.update(newCM("a", "uid-a", "4"))
	time.Sleep(300 * time.Millisecond)
	close(gate)

	e.waitCalls(3)
	time.Sleep(300 * time.Millisecond)
	cs := e.calls()
	if len(cs) != 3 {
		t.Fatalf("want Synchronization + in-flight + one folded call, got %d: %v", len(cs), cs)
	}
	if dataV(cs[2]) != "4" {
		t.Fatalf("folded call carries v=%q, want newest 4", dataV(cs[2]))
	}
}

// jshook.R10
// jshook.R25
func TestDispatcher_ThrowingHandle_WarnsRetriesWithoutRestart(t *testing.T) {
	const src = `
function handle(c) {
  record(c[0]);
  if (failNow()) { throw new Error("boom"); }
}`
	e := newEnv(t, src, jsengine.Limits{})
	e.fails.Store(2)
	before, _ := e.reg.Get(jsrun.HookKey(e.key))
	e.subscribe(corev1alpha1.HookBinding{})

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
	after, _ := e.reg.Get(jsrun.HookKey(e.key))
	if after != before || len(after.Recoveries.Recent) != 0 {
		t.Fatalf("script was prepared again after a thrown error (history %v)", after.Recoveries.Recent)
	}
	// R13: success ends the retries.
	time.Sleep(300 * time.Millisecond)
	if n := len(e.calls()); n != 3 {
		t.Fatalf("handle ran %d times, want 3 (retries must stop after success)", n)
	}
}

// jshook.R12
func TestDispatcher_MemoryLimit_WarnsKeepsVMAndRetries(t *testing.T) {
	const src = `
function handle(c) {
  if (oomNow()) { var a = []; while (true) { a.push(new Array(100000).fill(1)); } }
  record(c[0]);
}`
	e := newEnv(t, src, jsengine.Limits{MemoryMB: 1})
	e.ooms.Store(1)
	first, _ := e.reg.Get(jsrun.HookKey(e.key))
	e.subscribe(corev1alpha1.HookBinding{})

	e.waitEmitted(conditions.EventHandleFailed, 1)
	if ev := e.emitted()[0]; ev.Type != corev1.EventTypeWarning || ev.Message != "handle() exceeded its memory limit" {
		t.Fatalf("event %v, want the memory-limit Warning", ev)
	}
	cs := e.waitCalls(1) // the retry runs on a fresh VM of the same script
	if cs[0]["type"] != bcSynchronization {
		t.Fatalf("retried context %v", cs[0])
	}
	if now, _ := e.reg.Get(jsrun.HookKey(e.key)); now != first || len(now.Recoveries.Recent) != 0 {
		t.Fatal("the memory limit must not prepare the script again")
	}
	for _, ev := range e.emitted() {
		if ev.Reason == conditions.EventRestarted {
			t.Fatalf("memory limit must not restart the VM: %v", e.emitted())
		}
	}
}

// jshook.R13
func TestDispatcher_RetryKeepsFresherStateOfSameObject(t *testing.T) {
	const src = `
function handle(c) {
  enter();
  record(c[0]);
  if (c[0].type === "Event" && failNow()) {
    var end = Date.now() + 400;
    while (Date.now() < end) {}
    throw new Error("boom");
  }
}`
	e := newEnv(t, src, jsengine.Limits{}, newCM("a", "uid-a", "0"))
	e.fails.Store(1)
	e.subscribe(corev1alpha1.HookBinding{})
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
	if dataV(cs[1]) != "1" || dataV(cs[2]) != "2" {
		t.Fatalf("retry carries v=%q after failed v=%q, want the fresher 2", dataV(cs[2]), dataV(cs[1]))
	}
}

// jshook.R14
func TestDispatcher_Drop_NoMoreEvents(t *testing.T) {
	const src = `
function handle(c) { enter(); record(c[0]); }`
	e := newEnv(t, src, jsengine.Limits{})
	e.subscribe(corev1alpha1.HookBinding{})
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

// jshook.R11
// js-execution.R3
func TestDispatcher_Timeout_CancelsWarnsAndKeepsVM(t *testing.T) {
	const src = `
function handle(c) { enter(); while (true) {} }`
	e := newEnv(t, src, jsengine.Limits{TimeoutSeconds: 1})
	first, _ := e.reg.Get(jsrun.HookKey(e.key))
	e.subscribe(corev1alpha1.HookBinding{})

	e.waitEmitted(conditions.EventHandleTimeout, 1)
	for _, ev := range e.emitted() {
		if ev.Reason == conditions.EventHandleTimeout && ev.Type != corev1.EventTypeWarning {
			t.Fatalf("event %v is not a Warning", ev)
		}
		if ev.Reason == conditions.EventRestarted {
			t.Fatalf("a timeout must not restart the VM: %v", e.emitted())
		}
	}
	if now, _ := e.reg.Get(jsrun.HookKey(e.key)); now != first {
		t.Fatal("script was prepared again after the timeout")
	}
	// The retried event runs again, on a fresh VM of the same script.
	n := e.entered.Load()
	e.waitEntered(n)
}

// jshook.R9
func TestDispatcher_ResubscribeDuringCall_NoOverlapAndProcessAlive(t *testing.T) {
	const src = `
function handle(c) { enter(); var t = Date.now(); while (Date.now() - t < 300) {} record(c[0]); }`
	e := newEnv(t, src, jsengine.Limits{})
	e.subscribe(corev1alpha1.HookBinding{})
	e.waitEntered(0)

	// Re-subscribe while the first call is running: the old worker is stopped
	// and the new one must not start before the old call has ended.
	e.subscribe(corev1alpha1.HookBinding{})

	// The new subscription delivers its own Synchronization.
	deadline := time.Now().Add(10 * time.Second)
	for e.entered.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("new subscription never ran handle()")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.waitCalls(1)
}

// jshook.R12
// js-registry.R2
//
// A host function that panics makes the wasm call trap. The VM of that call
// is thrown away; the retried event runs on a fresh VM of the same prepared
// script, nothing is prepared again and no Restarted event is emitted.
func TestDispatcher_PanicInHandle_RetriesOnFreshVM(t *testing.T) {
	const src = `
function handle(c) { crashNow(); record(c[0]); }`
	e := newEnv(t, src, jsengine.Limits{})
	e.crashes.Store(1)
	first, _ := e.reg.Get(jsrun.HookKey(e.key))
	e.subscribe(corev1alpha1.HookBinding{})

	cs := e.waitCalls(1)
	if cs[0]["type"] != bcSynchronization {
		t.Fatalf("retried context %v", cs[0])
	}
	if e.crashes.Load() >= 0 {
		t.Fatal("crashNow() never panicked")
	}
	now, _ := e.reg.Get(jsrun.HookKey(e.key))
	if now != first || len(now.Recoveries.Recent) != 0 {
		t.Fatalf("a panic must not prepare the script again (history %v)", now.Recoveries.Recent)
	}
	for _, ev := range e.emitted() {
		if ev.Reason == conditions.EventRestarted {
			t.Fatalf("a panic must not emit Restarted: %v", e.emitted())
		}
	}
}

// jshook.R17
func TestDispatcher_SlowSync_DoesNotBlockOtherHooks(t *testing.T) {
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cmGVR: "ConfigMapList"})
	listing := make(chan struct{}, 1024)
	// A list carrying hook A's object selector always fails: the informer of
	// hook A never syncs. (Blocking inside the reactor would stall the whole
	// fake client.)
	dyn.PrependReactor("list", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		la, ok := a.(clienttesting.ListAction)
		if !ok || !strings.Contains(la.GetListRestrictions().Labels.String(), "slow") {
			return false, nil, nil
		}
		select {
		case listing <- struct{}{}:
		default:
		}
		return true, nil, errors.New("apiserver unreachable")
	})
	d := dispatcher.New(dyn, fixedMapper{}, jsregistry.NewRegistry())
	keyA := types.NamespacedName{Name: "a"}
	keyB := types.NamespacedName{Name: "b"}
	off := false
	bindingsA := []corev1alpha1.HookBinding{{Name: "cms", ResourceRule: cmRule, ObjectMatch: corev1alpha1.ObjectMatch{
		ObjectSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"sync": "slow"}},
	}}}
	bindingsB := []corev1alpha1.HookBinding{{Name: "cms", ResourceRule: cmRule, Synchronization: &off}}
	t.Cleanup(func() { d.Drop(keyA); d.Drop(keyB) })

	errA := make(chan error, 1)
	go func() { errA <- d.Subscribe(context.Background(), keyA, bindingsA, nil, nil) }()
	select {
	case <-listing:
	case <-time.After(5 * time.Second):
		t.Fatal("hook A never started listing")
	}

	done := make(chan error, 1)
	go func() {
		if err := d.Subscribe(context.Background(), keyB, bindingsB, nil, nil); err != nil {
			done <- err
			return
		}
		d.Drop(keyB)
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Subscribe B: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Subscribe/Drop of hook B blocked behind the unsynced informer of hook A")
	}

	// Drop of A itself must also return while its sync hangs, and end the
	// pending Subscribe.
	dropped := make(chan struct{})
	go func() { d.Drop(keyA); close(dropped) }()
	select {
	case <-dropped:
	case <-time.After(3 * time.Second):
		t.Fatal("Drop of hook A blocked")
	}
	select {
	case <-errA:
	case <-time.After(3 * time.Second):
		t.Fatal("Subscribe A did not return after Drop")
	}
}

// While the hook has no prepared script (its first build runs) events are
// kept and retried with the rate limiter; they reach the script once it is
// prepared, none is dropped.
//
// jshook.R19
// js-registry.R19
func TestDispatcher_NoVM_KeepsEventsAndDeliversAfterBuild(t *testing.T) {
	const src = `
function handle(c) { enter(); record(c[0]); }`
	e := newEnv(t, src, jsengine.Limits{})
	gate := make(chan struct{})
	e.hold.Store(&gate)
	// Forget the script and register it again: the new build blocks on the gate.
	e.reg.Drop(jsrun.HookKey(e.key))
	if st := e.reg.Ensure(jsrun.HookKey(e.key), e.spec); st.Phase != jsrun.PhasePreparing {
		t.Fatalf("state %v, want Preparing", st)
	}
	if _, ok := e.reg.Get(jsrun.HookKey(e.key)); ok {
		t.Fatal("a building hook must hold no script")
	}
	e.subscribe(corev1alpha1.HookBinding{})

	time.Sleep(300 * time.Millisecond)
	if n := e.entered.Load(); n != 0 {
		t.Fatalf("handle ran %d times without a VM", n)
	}
	close(gate)
	cs := e.waitCalls(1)
	if cs[0]["type"] != bcSynchronization {
		t.Fatalf("event kept across the rebuild: %v", cs[0])
	}
}

// A script explains itself. console.warn and console.error reach the person
// who deployed the hook as a Warning event on the hook's own CR; quieter
// levels stay in the operator log.
//
// js-execution.R17
func TestDispatcher_ConsoleWarning_BecomesAnEvent(t *testing.T) {
	const src = `
function handle(c) {
  console.debug("quiet");
  console.log("also quiet");
  console.error("image tag 'latest' is not allowed");
  record(c[0]);
}`
	e := newEnv(t, src, jsengine.Limits{})
	e.subscribe(corev1alpha1.HookBinding{})

	e.waitEmitted(conditions.EventScriptMessage, 1)
	events := e.emitted()
	seen := make([]string, 0, len(events))
	for _, ev := range events {
		if ev.Reason != conditions.EventScriptMessage {
			continue
		}
		if ev.Type != corev1.EventTypeWarning {
			t.Errorf("a console.error must be a Warning, got %q", ev.Type)
		}
		seen = append(seen, ev.Message)
	}
	if len(seen) == 0 || seen[0] != "image tag 'latest' is not allowed" {
		t.Errorf("the script's own words did not reach the CR: %v", seen)
	}
	for _, m := range seen {
		if strings.Contains(m, "quiet") {
			t.Errorf("console.log/debug must not become an Event: %q", m)
		}
	}
}

// A script in a hot loop must not fill etcd with its own Events.
//
// js-execution.R17
//
// js-execution.R17
func TestDispatcher_ConsoleEventsAreCappedPerCall(t *testing.T) {
	const src = `
function handle(c) {
  for (var i = 0; i < 20; i++) console.warn("noisy " + i);
  record(c[0]);
}`
	e := newEnv(t, src, jsengine.Limits{})
	e.subscribe(corev1alpha1.HookBinding{})
	e.waitCalls(1)
	e.waitEmitted(conditions.EventScriptMessage, 1)

	n := 0
	for _, ev := range e.emitted() {
		if ev.Reason == conditions.EventScriptMessage {
			n++
		}
	}
	if n > jslog.MaxVisibleEvents {
		t.Errorf("one call produced %d Events, cap is %d", n, jslog.MaxVisibleEvents)
	}
}

// jshook.R21
func TestPlanWatches_RejectsWildcardUnknownAndDuplicate(t *testing.T) {
	cases := []struct {
		name     string
		bindings []corev1alpha1.HookBinding
		want     string
	}{
		{
			"wildcard resource",
			[]corev1alpha1.HookBinding{{Name: "a", ResourceRule: corev1alpha1.ResourceRule{
				APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"*"},
			}}},
			"is not allowed in a hook binding",
		},
		{
			"wildcard group",
			[]corev1alpha1.HookBinding{{Name: "a", ResourceRule: corev1alpha1.ResourceRule{
				APIGroups: []string{"*"}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"},
			}}},
			"is not allowed in a hook binding",
		},
		{
			"unknown resource",
			[]corev1alpha1.HookBinding{{Name: "a", ResourceRule: corev1alpha1.ResourceRule{
				APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"widgets"},
			}}},
			`no resource "widgets"`,
		},
		{
			"two bindings on the same resource",
			[]corev1alpha1.HookBinding{
				{Name: "a", ResourceRule: cmRule},
				{Name: "b", ResourceRule: cmRule},
			},
			"already watches",
		},
		{
			"bad object selector",
			[]corev1alpha1.HookBinding{{Name: "a", ResourceRule: cmRule, ObjectMatch: corev1alpha1.ObjectMatch{
				ObjectSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "x", Operator: "Bogus"},
				}},
			}}},
			"objectSelector",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{cmGVR: "ConfigMapList"})
			d := dispatcher.New(dyn, fixedMapper{}, jsregistry.NewRegistry())
			key := types.NamespacedName{Name: "h"}
			t.Cleanup(func() { d.Drop(key) })

			err := d.Subscribe(context.Background(), key, tc.bindings, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Subscribe error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// A rejected re-subscribe must not cost the hook the watches it already has:
// everything that can fail is resolved before the old subscription is torn
// down.
//
// jshook.R20
func TestPlanWatches_BadBindingKeepsExistingWatches(t *testing.T) {
	const src = `function handle(c) { record(c[0]); }`
	e := newEnv(t, src, jsengine.Limits{})
	e.subscribe(corev1alpha1.HookBinding{})
	e.waitCalls(1) // the Synchronization context

	bad := []corev1alpha1.HookBinding{{Name: "a", ResourceRule: corev1alpha1.ResourceRule{
		APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"widgets"},
	}}}
	if err := e.d.Subscribe(context.Background(), e.key, bad, nil, e.emit); err == nil {
		t.Fatal("Subscribe with an unknown resource must fail")
	}

	e.create(newCM("still-watched", "uid-still", "1"))
	cs := e.waitCalls(2)
	if got := cs[1]["type"]; got != bcEvent {
		t.Fatalf("after the rejected re-subscribe the old watch is gone: %v", cs[1])
	}
}

// The informers of a hook list and watch through the client handed to
// Subscribe, the client of the hook's ServiceAccount, not through the
// dispatcher's own.
// kube-access.R12
func TestDispatcher_WatchesRunAsTheHooksClient(t *testing.T) {
	e := newEnv(t, recordSrc, jsengine.Limits{}, newCM("a", "uid-a", "1"))
	own := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cmGVR: "ConfigMapList"})
	e.d = dispatcher.New(own, fixedMapper{}, e.reg)

	b := corev1alpha1.HookBinding{Name: "cms", ResourceRule: cmRule}
	if err := e.d.Subscribe(context.Background(), e.key, []corev1alpha1.HookBinding{b}, e.dyn, e.emit); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cs := e.waitCalls(1)
	objs, _ := cs[0]["objects"].([]any)
	if len(objs) != 1 {
		t.Fatalf("synchronization has %d objects, want the one only the hook's client sees: %v", len(objs), cs[0])
	}
}
