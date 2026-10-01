#!/usr/bin/env bash
# Builds engine.wasm: QuickJS-ng plus glue.c as a WASI reactor.
#
#   WASI_SDK=/path/to/wasi-sdk QUICKJS=/path/to/quickjs-ng ./build.sh
#
# wasm-opt (binaryen) is optional; without it the module stays unoptimised.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
: "${WASI_SDK:?set WASI_SDK to a wasi-sdk directory}"
: "${QUICKJS:?set QUICKJS to a quickjs-ng checkout}"
out=${OUT:-$here/../engine.wasm}

"$WASI_SDK/bin/clang" \
  --target=wasm32-wasip1 --sysroot="$WASI_SDK/share/wasi-sysroot" \
  -O2 -flto -DNDEBUG -D_GNU_SOURCE \
  -D_WASI_EMULATED_PROCESS_CLOCKS -D_WASI_EMULATED_SIGNAL \
  -Wno-everything \
  -I"$QUICKJS" \
  "$QUICKJS/quickjs.c" "$QUICKJS/libregexp.c" "$QUICKJS/libunicode.c" "$QUICKJS/dtoa.c" \
  "$here/glue.c" \
  -mexec-model=reactor \
  -Wl,--stack-first -Wl,-z,stack-size=1048576 \
  -Wl,--strip-all \
  -lwasi-emulated-process-clocks -lwasi-emulated-signal \
  -o "$out"

if command -v wasm-opt >/dev/null; then
  wasm-opt -O3 --enable-bulk-memory --enable-sign-ext --enable-nontrapping-float-to-int \
    --enable-mutable-globals --enable-multivalue --enable-reference-types \
    "$out" -o "$out"
fi
ls -l "$out"
