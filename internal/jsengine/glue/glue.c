// glue.c is the whole ABI between Go and QuickJS-ng.
//
// One wasm instance holds one JSRuntime with one JSContext. Data crosses the
// boundary as bytes in linear memory: Go writes input with gj_alloc, reads
// output through gj_out_ptr/gj_out_len. Values cross as JSON, so Go never
// holds a JSValue.
//
// All state lives in linear memory, so the host snapshots an instance after
// the script is loaded and restores a fresh instance from it for every call.
// After a restore it calls gj_update_stack_top and gj_reseed (Math.random).
//
// Status codes of every call: 0 ok, 1 JS exception, 2 interrupted,
// 3 out of memory, 4 bad input.
//
// Imports (module "env"):
//   interrupt() -> i32    non-zero stops the running call
//   host_call(name, nlen, arg, alen) -> i32
//                         runs the host function `name` with the JSON `arg`;
//                         returns the length of the JSON result (0 = undefined),
//                         or -(n+1) for an error whose message is n bytes
//   host_read(dst)        copies the pending result or message to dst
// JavaScript reaches host_call through the global function __gj_host(name,
// arg), which the Go side wraps and deletes (see jsengine/host.go).

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "quickjs.h"

#define EXPORT(name) __attribute__((export_name(#name)))

enum { GJ_OK = 0, GJ_EXC = 1, GJ_INTERRUPTED = 2, GJ_OOM = 3, GJ_BADINPUT = 4 };

// The host answers non-zero when the current call must stop. QuickJS asks it
// at backward jumps and function calls, every JS_INTERRUPT_COUNTER_INIT ops.
__attribute__((import_module("env"), import_name("interrupt")))
int host_interrupt(void);

__attribute__((import_module("env"), import_name("host_call")))
int32_t host_call(const char *name, uint32_t nlen, const char *arg, uint32_t alen);

__attribute__((import_module("env"), import_name("host_read")))
void host_read(char *dst);

static JSRuntime *rt;
static JSContext *ctx;
static uint8_t *out_buf;
static size_t out_len;

// random_state drives Math.random. It lives in linear memory like all other
// state, so every VM restored from one snapshot starts with the same value;
// the host reseeds it after each restore (gj_reseed).
static uint64_t random_state = 1;

static uint64_t xorshift64star(void) {
    uint64_t x = random_state;
    x ^= x >> 12;
    x ^= x << 25;
    x ^= x >> 27;
    random_state = x;
    return x * 0x2545F4914F6CDD1DULL;
}

// js_random replaces Math.random: a double in [0, 1) from 52 random bits.
static JSValue js_random(JSContext *c, JSValueConst this_val, int argc, JSValueConst *argv) {
    (void)this_val;
    (void)argc;
    (void)argv;
    return JS_NewFloat64(c, (double)(xorshift64star() >> 12) * 0x1.0p-52);
}

static int on_interrupt(JSRuntime *r, void *opaque) {
    (void)r;
    (void)opaque;
    return host_interrupt();
}

// js_host is __gj_host(name, arg): arg crosses as JSON, so is the result.
static JSValue js_host(JSContext *c, JSValueConst this_val, int argc, JSValueConst *argv) {
    (void)this_val;
    if (argc < 1)
        return JS_ThrowTypeError(c, "host call: name required");
    size_t nlen;
    const char *name = JS_ToCStringLen(c, &nlen, argv[0]);
    if (!name)
        return JS_EXCEPTION;
    JSValue js = JS_UNDEFINED;
    const char *arg = "";
    size_t alen = 0;
    if (argc > 1 && !JS_IsUndefined(argv[1])) {
        js = JS_JSONStringify(c, argv[1], JS_UNDEFINED, JS_UNDEFINED);
        if (JS_IsException(js)) {
            JS_FreeCString(c, name);
            return JS_EXCEPTION;
        }
        if (!JS_IsUndefined(js)) {
            arg = JS_ToCStringLen(c, &alen, js);
            if (!arg) {
                JS_FreeValue(c, js);
                JS_FreeCString(c, name);
                return JS_EXCEPTION;
            }
        }
    }
    int32_t n = host_call(name, (uint32_t)nlen, arg, (uint32_t)alen);
    if (!JS_IsUndefined(js)) {
        JS_FreeCString(c, arg);
        JS_FreeValue(c, js);
    }
    JS_FreeCString(c, name);
    int is_err = n < 0;
    size_t len = is_err ? (size_t)(-(int64_t)n - 1) : (size_t)n;
    if (len == 0)
        return is_err ? JS_ThrowPlainError(c, "host call failed") : JS_UNDEFINED;
    char *buf = js_malloc(c, len + 1);
    if (!buf)
        return JS_EXCEPTION;
    host_read(buf);
    buf[len] = 0;
    JSValue r;
    if (is_err)
        r = JS_ThrowPlainError(c, "%s", buf);
    else
        r = JS_ParseJSON(c, buf, len, "<host>");
    js_free(c, buf);
    return r;
}

static void set_out(const void *p, size_t n) {
    free(out_buf);
    out_buf = NULL;
    out_len = 0;
    if (n == 0)
        return;
    out_buf = malloc(n);
    if (!out_buf)
        return;
    memcpy(out_buf, p, n);
    out_len = n;
}

static void set_out_js_string(JSValueConst v) {
    size_t n;
    const char *s = JS_ToCStringLen(ctx, &n, v);
    if (!s) {
        JS_FreeValue(ctx, JS_GetException(ctx)); // toString threw: no output
        set_out(NULL, 0);
        return;
    }
    set_out(s, n);
    JS_FreeCString(ctx, s);
}

// take_exception moves the pending exception into the output buffer as
// "message\nstack" and classifies it.
static int take_exception(void) {
    JSValue exc = JS_GetException(ctx);
    int code = GJ_EXC;
    if (JS_IsUncatchableError(exc)) {
        code = GJ_INTERRUPTED;
    }
    const char *msg = JS_ToCString(ctx, exc);
    if (msg && strcmp(msg, "InternalError: out of memory") == 0)
        code = GJ_OOM;
    if (JS_IsNull(exc))
        code = GJ_OOM; // QuickJS throws null when it cannot allocate the error
    if (code == GJ_EXC && JS_IsError(exc)) {
        JSValue stack = JS_GetPropertyStr(ctx, exc, "stack");
        const char *st = JS_ToCString(ctx, stack);
        size_t ml = msg ? strlen(msg) : 0, sl = st ? strlen(st) : 0;
        char *buf = malloc(ml + 1 + sl);
        if (buf) {
            if (ml)
                memcpy(buf, msg, ml);
            buf[ml] = '\n';
            if (sl)
                memcpy(buf + ml + 1, st, sl);
            set_out(buf, ml + 1 + sl);
            free(buf);
        }
        if (st)
            JS_FreeCString(ctx, st);
        JS_FreeValue(ctx, stack);
    } else if (msg) {
        set_out(msg, strlen(msg));
    } else {
        set_out(NULL, 0);
    }
    if (msg)
        JS_FreeCString(ctx, msg);
    JS_FreeValue(ctx, exc);
    return code;
}

EXPORT(gj_alloc) void *gj_alloc(uint32_t n) { return malloc(n); }
EXPORT(gj_free) void gj_free(void *p) { free(p); }
EXPORT(gj_out_ptr) uint8_t *gj_out_ptr(void) { return out_buf; }
EXPORT(gj_out_len) uint32_t gj_out_len(void) { return (uint32_t)out_len; }

// gj_init builds the runtime. mem_limit 0 means no limit.
EXPORT(gj_init) int gj_init(uint32_t mem_limit, uint32_t stack_size) {
    rt = JS_NewRuntime();
    if (!rt)
        return GJ_OOM;
    if (mem_limit)
        JS_SetMemoryLimit(rt, mem_limit);
    if (stack_size)
        JS_SetMaxStackSize(rt, stack_size);
    JS_SetInterruptHandler(rt, on_interrupt, NULL);
    ctx = JS_NewContext(rt);
    if (!ctx)
        return GJ_OOM;
    JSValue global = JS_GetGlobalObject(ctx);
    JS_SetPropertyStr(ctx, global, "__gj_host", JS_NewCFunction(ctx, js_host, "__gj_host", 2));
    JSValue math = JS_GetPropertyStr(ctx, global, "Math");
    JS_DefinePropertyValueStr(ctx, math, "random", JS_NewCFunction(ctx, js_random, "random", 0),
                              JS_PROP_WRITABLE | JS_PROP_CONFIGURABLE);
    JS_FreeValue(ctx, math);
    JS_FreeValue(ctx, global);
    return GJ_OK;
}

// gj_reseed sets the state of Math.random (0 becomes 1: xorshift needs a
// non-zero state). The host calls it after every restore from a snapshot.
EXPORT(gj_reseed) void gj_reseed(uint64_t seed) { random_state = seed ? seed : 1; }

// gj_update_stack_top re-reads the C stack top. The stack pointer is back at
// its base between calls, but a restored snapshot must not trust the old one.
EXPORT(gj_update_stack_top) void gj_update_stack_top(void) { JS_UpdateStackTop(rt); }

// gj_eval runs source as a global script; function declarations become
// globals. The output is the string form of the last expression (empty for
// undefined). The source must be NUL-terminated at src[len].
EXPORT(gj_eval) int gj_eval(const char *src, uint32_t len, const char *name) {
    JSValue v = JS_Eval(ctx, src, len, name, JS_EVAL_TYPE_GLOBAL);
    if (JS_IsException(v))
        return take_exception();
    if (JS_IsUndefined(v))
        set_out(NULL, 0);
    else
        set_out_js_string(v);
    JS_FreeValue(ctx, v);
    return GJ_OK;
}

// gj_has_export reports whether the global `name` is a function.
EXPORT(gj_has_export) int gj_has_export(const char *name) {
    JSValue global = JS_GetGlobalObject(ctx);
    JSValue fn = JS_GetPropertyStr(ctx, global, name);
    int ok = JS_IsFunction(ctx, fn);
    JS_FreeValue(ctx, fn);
    JS_FreeValue(ctx, global);
    return ok;
}

// gj_call calls the global function name with one argument parsed from JSON
// (none if len is 0) and writes the JSON of its result to the output buffer
// (empty for undefined). The JSON must be NUL-terminated at arg[len].
EXPORT(gj_call) int gj_call(const char *name, const char *arg, uint32_t len) {
    JSValue global = JS_GetGlobalObject(ctx);
    JSValue fn = JS_GetPropertyStr(ctx, global, name);
    if (!JS_IsFunction(ctx, fn)) {
        JS_FreeValue(ctx, fn);
        JS_FreeValue(ctx, global);
        set_out("not a function", 14);
        return GJ_BADINPUT;
    }
    JSValue a = JS_UNDEFINED;
    if (len > 0) {
        a = JS_ParseJSON(ctx, arg, len, "<arg>");
        if (JS_IsException(a)) {
            JS_FreeValue(ctx, fn);
            JS_FreeValue(ctx, global);
            return take_exception();
        }
    }
    JSValue r = JS_Call(ctx, fn, global, len > 0 ? 1 : 0, &a);
    JS_FreeValue(ctx, a);
    JS_FreeValue(ctx, fn);
    JS_FreeValue(ctx, global);
    if (JS_IsException(r))
        return take_exception();
    if (JS_IsUndefined(r)) {
        set_out(NULL, 0);
        return GJ_OK;
    }
    JSValue s = JS_JSONStringify(ctx, r, JS_UNDEFINED, JS_UNDEFINED);
    JS_FreeValue(ctx, r);
    if (JS_IsException(s))
        return take_exception();
    set_out_js_string(s);
    JS_FreeValue(ctx, s);
    return GJ_OK;
}

// gj_mem_used reports the bytes QuickJS has allocated.
EXPORT(gj_mem_used) uint32_t gj_mem_used(void) {
    JSMemoryUsage u;
    JS_ComputeMemoryUsage(rt, &u);
    return (uint32_t)u.malloc_size;
}
