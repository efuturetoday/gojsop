#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { binaryPath, notFoundHint } from "./index.js";

const bin = binaryPath();
const r = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (r.error) {
  const code = (r.error as NodeJS.ErrnoException).code;
  console.error(code === "ENOENT" ? notFoundHint(bin) : `cannot start "${bin}": ${r.error.message}`);
  process.exit(1);
}
process.exit(r.status ?? 1);
