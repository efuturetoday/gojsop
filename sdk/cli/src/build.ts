import fs from "node:fs";
import path from "node:path";
import { bundle, isModule, scriptBeside } from "./bundle.js";

const MANIFESTS = ["policy.yaml", "hook.yaml"];
const SKIP = new Set(["node_modules", "dist"]);

function* manifestDirs(dir: string): Generator<string> {
  const entries = fs.readdirSync(dir, { withFileTypes: true });
  if (entries.some((e) => e.isFile() && MANIFESTS.includes(e.name))) yield dir;
  for (const e of entries) {
    if (e.isDirectory() && !SKIP.has(e.name) && !e.name.startsWith(".")) {
      yield* manifestDirs(path.join(dir, e.name));
    }
  }
}

/**
 * gojsop build [dir...]: writes the script of every hook and policy below
 * the directories (default: the current one) to dist/<name>.js next to its
 * manifest, the file kustomize ships as the script's ConfigMap. A
 * TypeScript or module script is bundled; a plain .js script is copied.
 */
export async function build(roots: string[]): Promise<number> {
  let built = 0;
  for (const root of roots.length ? roots : ["."]) {
    for (const dir of manifestDirs(root)) {
      const entry = scriptBeside(dir);
      if (!entry) continue;
      const code = isModule(entry) ? (await bundle(entry)).code : fs.readFileSync(entry, "utf8");
      const out = path.join(dir, "dist", path.basename(entry).replace(/\.[^.]+$/, ".js"));
      fs.mkdirSync(path.dirname(out), { recursive: true });
      fs.writeFileSync(out, code);
      console.log(`built ${path.relative(process.cwd(), out)}`);
      built++;
    }
  }
  return built;
}
