import fs from "node:fs";
import path from "node:path";
import type { Plugin } from "vite";

// Names the IIFE that holds the module's exports while it runs.
const EXPORTS = "__gojsop_exports";

/** Script files gojsop bundles; a .js file is a plain script and runs as it is. */
const MODULE = /\.(ts|mts|mjs)$/;
const NOT_A_SCRIPT = /\.(test|spec)\.[cm]?[jt]s$|\.d\.ts$/;

/**
 * The script next to a manifest: policy.ts or hook.ts (or .mts, .mjs, .js)
 * named like the manifest, else the one script file of the directory that
 * is not a test. Other files are modules the script may import.
 * Undefined when there is none.
 */
export function scriptBeside(manifestDir: string): string | undefined {
  for (const base of ["policy", "hook"]) {
    if (!fs.existsSync(path.join(manifestDir, `${base}.yaml`))) continue;
    for (const ext of [".ts", ".mts", ".mjs", ".js"]) {
      const f = path.join(manifestDir, base + ext);
      if (fs.existsSync(f)) return f;
    }
  }
  const found = fs
    .readdirSync(manifestDir)
    .filter((f) => /\.([cm]?[jt]s)$/.test(f) && !NOT_A_SCRIPT.test(f))
    .sort();
  if (found.length > 1) {
    throw new Error(`${manifestDir}: several scripts (${found.join(", ")}); name the entry policy.ts or hook.ts`);
  }
  return found[0] && path.join(manifestDir, found[0]);
}

/** Whether a script is a module that bundle() must turn into a plain script. */
export function isModule(file: string): boolean {
  return MODULE.test(file);
}

// After the bundle runs, its exports become globals: gojsop calls
// validate, mutate and handle as properties of globalThis.
function exportsToGlobals(): Plugin {
  return {
    name: "gojsop:exports-to-globals",
    generateBundle(_options, bundle) {
      for (const chunk of Object.values(bundle)) {
        if (chunk.type !== "chunk" || !chunk.isEntry) continue;
        // Region markers name local paths, and the script ends up in the
        // cluster. Blank them but keep the lines, so the source map holds.
        chunk.code = chunk.code.replace(/^[ \t]*\/\/#(end)?region.*$/gm, "");
        chunk.code += `\n${chunk.exports.map((n) => `globalThis.${n} = ${EXPORTS}.${n};\n`).join("")}`;
      }
    },
  };
}

// Vite writes sources relative to its output directory, dist/ of root.
function absoluteSources(map: NonNullable<Bundle["map"]>, root: string): Bundle["map"] {
  return { ...map, sources: map.sources.map((s) => path.resolve(root, "dist", s)) };
}

/** A bundled script, the files it was built from, and its source map. */
export interface Bundle {
  code: string;
  files: string[];
  /** Source map (v3) with absolute paths in sources. */
  map?: { version: number; sources: string[]; mappings: string; names: string[] };
}

/**
 * Bundles a TypeScript or ES-module script, with its imports, into the one
 * plain script gojsop runs: ES2023, no modules, the exports as globals.
 * Uses the project's Vite.
 */
export async function bundle(file: string): Promise<Bundle> {
  const { build } = await import("vite");
  const entry = path.resolve(file); // vite resolves a relative entry against root
  const out = await build({
    configFile: false,
    logLevel: "silent",
    root: path.dirname(entry),
    plugins: [exportsToGlobals()],
    build: {
      write: false,
      target: "es2023",
      minify: false,
      sourcemap: "hidden",
      emptyOutDir: false,
      lib: { entry, formats: ["iife"], name: EXPORTS, fileName: "script" },
    },
  });
  const outputs = Array.isArray(out) ? out : [out];
  for (const o of outputs) {
    if ("output" in o) {
      const chunk = o.output.find((c) => c.type === "chunk" && c.isEntry);
      if (chunk && chunk.type === "chunk") {
        return {
          code: chunk.code,
          files: chunk.moduleIds.filter((id) => path.isAbsolute(id)),
          map: chunk.map ? absoluteSources(JSON.parse(chunk.map.toString()), path.dirname(entry)) : undefined,
        };
      }
    }
  }
  throw new Error(`${entry}: vite produced no script`);
}
