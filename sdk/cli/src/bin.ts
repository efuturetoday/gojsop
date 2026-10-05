#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { build } from "./build.js";
import { bundle, isModule, scriptBeside } from "./bundle.js";
import { binaryPath, notFoundHint } from "./index.js";

const args = process.argv.slice(2);

// build runs here, in Node, because it bundles with the project's Vite;
// every other command is the gojsop binary's.
if (args[0] === "build") {
  try {
    await build(args.slice(1));
    process.exit(0);
  } catch (e) {
    console.error(e instanceof Error ? e.message : e);
    process.exit(1);
  }
}

// gojsop run on a TypeScript script: bundle it here and hand the result to
// the binary with --source, as @gojsop/testing does for every test call.
if (args[0] === "run" && args[1] && !args.includes("--source")) {
  try {
    const st = fs.statSync(args[1], { throwIfNoEntry: false });
    const entry = st && scriptBeside(st.isDirectory() ? args[1] : path.dirname(args[1]));
    if (entry && isModule(entry)) {
      const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "gojsop-")), "script.js");
      fs.writeFileSync(file, (await bundle(entry)).code);
      args.push("--source", file);
    }
  } catch (e) {
    console.error(e instanceof Error ? e.message : e);
    process.exit(1);
  }
}

const bin = binaryPath();
const r = spawnSync(bin, args, { stdio: "inherit" });
if (r.error) {
  const code = (r.error as NodeJS.ErrnoException).code;
  console.error(code === "ENOENT" ? notFoundHint(bin) : `cannot start "${bin}": ${r.error.message}`);
  process.exit(1);
}
process.exit(r.status ?? 1);
