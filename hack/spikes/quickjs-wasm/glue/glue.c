// glue.c is the whole ABI between Go and QuickJS-ng in this spike.
//
// One wasm instance holds one JSRuntime with one JSContext. Data crosses the
// boundary as bytes in linear memory: Go writes input with gj_alloc, reads
// output through gj_out_ptr/gj_out_len. Values cross as JSON, so Go never
// holds a JSValue.
//
// Status codes of every call: 0 ok, 1 JS exception, 2 interrupted,
// 3 out of memory, 4 bad input.

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

static JSRuntime *rt;
static JSContext *ctx;
static uint8_t *out_buf;
static size_t out_len;

static int on_interrupt(JSRuntime *r, void *opaque) {
    (void)r;
    (void)opaque;
    return host_interrupt();
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
    return GJ_OK;
}

// gj_update_stack_top re-reads the C stack top. The stack pointer is back at
// its base between calls, but a restored snapshot must not trust the old one.
EXPORT(gj_update_stack_top) void gj_update_stack_top(void) { JS_UpdateStackTop(rt); }

// gj_eval runs source as a global script; function declarations become
// globals. The source must be NUL-terminated at src[len].
EXPORT(gj_eval) int gj_eval(const char *src, uint32_t len, const char *name) {
    JSValue v = JS_Eval(ctx, src, len, name, JS_EVAL_TYPE_GLOBAL);
    if (JS_IsException(v))
        return take_exception();
    JS_FreeValue(ctx, v);
    set_out(NULL, 0);
    return GJ_OK;
}

// gj_compile compiles source without running it and writes its bytecode to
// the output buffer. strip 1 drops the source text (Function.prototype.toString
// then fails), line numbers stay.
EXPORT(gj_compile) int gj_compile(const char *src, uint32_t len, const char *name, int strip) {
    JSValue fn = JS_Eval(ctx, src, len, name, JS_EVAL_TYPE_GLOBAL | JS_EVAL_FLAG_COMPILE_ONLY);
    if (JS_IsException(fn))
        return take_exception();
    size_t n;
    uint8_t *bc = JS_WriteObject(ctx, &n, fn, JS_WRITE_OBJ_BYTECODE | (strip ? JS_WRITE_OBJ_STRIP_SOURCE : 0));
    JS_FreeValue(ctx, fn);
    if (!bc)
        return take_exception();
    set_out(bc, n);
    js_free(ctx, bc);
    return GJ_OK;
}

// gj_eval_bytecode runs bytecode from gj_compile. Only trusted input: the
// bytecode reader does not validate.
EXPORT(gj_eval_bytecode) int gj_eval_bytecode(const uint8_t *bc, uint32_t len) {
    JSValue fn = JS_ReadObject(ctx, bc, len, JS_READ_OBJ_BYTECODE);
    if (JS_IsException(fn))
        return take_exception();
    JSValue v = JS_EvalFunction(ctx, fn);
    if (JS_IsException(v))
        return take_exception();
    JS_FreeValue(ctx, v);
    set_out(NULL, 0);
    return GJ_OK;
}

// gj_call calls the global function name with one argument parsed from JSON
// and writes the JSON of its result to the output buffer (empty for
// undefined). The JSON must be NUL-terminated at arg[len].
EXPORT(gj_call) int gj_call(const char *name, const char *arg, uint32_t len) {
    JSValue global = JS_GetGlobalObject(ctx);
    JSValue fn = JS_GetPropertyStr(ctx, global, name);
    if (!JS_IsFunction(ctx, fn)) {
        JS_FreeValue(ctx, fn);
        JS_FreeValue(ctx, global);
        set_out("not a function", 14);
        return GJ_BADINPUT;
    }
    JSValue a = JS_ParseJSON(ctx, arg, len, "<arg>");
    if (JS_IsException(a)) {
        JS_FreeValue(ctx, fn);
        JS_FreeValue(ctx, global);
        return take_exception();
    }
    JSValue r = JS_Call(ctx, fn, global, 1, &a);
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
