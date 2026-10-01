#!/usr/bin/env bash
# Builds ../engine.wasm: QuickJS-ng plus glue.c as a WASI reactor. `make
# engine-wasm` downloads the pinned toolchain (versions.env) into ./bin and
# runs this script; run it by hand with:
#
#   WASI_SDK=... QUICKJS=... WASM_OPT=... ./build.sh
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
: "${WASI_SDK:?set WASI_SDK to a wasi-sdk directory}"
: "${QUICKJS:?set QUICKJS to a quickjs-ng checkout}"
: "${WASM_OPT:?set WASM_OPT to the wasm-opt binary of binaryen}"
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

"$WASM_OPT" -O3 --enable-bulk-memory --enable-sign-ext --enable-nontrapping-float-to-int \
  --enable-mutable-globals --enable-multivalue --enable-reference-types \
  "$out" -o "$out"
ls -l "$out"
