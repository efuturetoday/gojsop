#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { parseArgs } from "node:util";
import { build } from "./build.js";
import { bundle, isModule, scriptBeside } from "./bundle.js";
import { binaryPath, notFoundHint } from "./index.js";
import { create, remove, rename } from "./scaffold.js";

const usage = `usage:
  gojsop new <policy|hook> <name>     add a policy or hook with a passing test
  gojsop rn <name> <new-name>         rename one
  gojsop rm <name> [--yes]            remove one
  gojsop build                        write dist/ for kubectl apply -f dist/
  gojsop run <manifest> (--request <file> | --event <file>) [--cluster <file>] [--trace]
  gojsop serve --stdio
`;

const args = process.argv.slice(2);

// These commands run here, in Node: they edit the workspace or bundle with
// the project's Vite. run and serve are the gojsop binary's.
const OWN = new Set([undefined, "help", "-h", "--help", "new", "rm", "remove", "rn", "rename", "build"]);

async function workspaceCommand(): Promise<boolean> {
  const [cmd, ...rest] = args;
  if (!OWN.has(cmd)) return false;
  const { values, positionals } = parseArgs({
    args: rest,
    allowPositionals: true,
    options: { yes: { type: "boolean", short: "y" } },
  });
  const want = (n: number) => {
    if (positionals.length !== n) throw new Error(usage);
  };
  switch (cmd) {
    case undefined:
    case "help":
    case "-h":
    case "--help":
      console.log(usage);
      return true;
    case "new":
      want(2);
      create(positionals[0], positionals[1]);
      return true;
    case "rm":
    case "remove":
      want(1);
      await remove(positionals[0], values.yes ?? false);
      return true;
    case "rn":
    case "rename":
      want(2);
      rename(positionals[0], positionals[1]);
      return true;
    case "build":
      want(0);
      await build();
      return true;
  }
  return false;
}

try {
  if (await workspaceCommand()) process.exit(0);
  // gojsop run on a TypeScript script: bundle it here and hand the result
  // to the binary with --source, as @gojsop/testing does for every call.
  if (args[0] === "run" && args[1] && !args.includes("--source")) {
    const st = fs.statSync(args[1], { throwIfNoEntry: false });
    const entry = st && scriptBeside(st.isDirectory() ? args[1] : path.dirname(args[1]));
    if (entry && isModule(entry)) {
      const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "gojsop-")), "script.js");
      fs.writeFileSync(file, (await bundle(entry)).code);
      args.push("--source", file);
    }
  }
} catch (e) {
  console.error(e instanceof Error ? e.message : e);
  process.exit(1);
}

const bin = binaryPath();
const r = spawnSync(bin, args, { stdio: "inherit" });
if (r.error) {
  const code = (r.error as NodeJS.ErrnoException).code;
  console.error(code === "ENOENT" ? notFoundHint(bin) : `cannot start "${bin}": ${r.error.message}`);
  process.exit(1);
}
process.exit(r.status ?? 1);
