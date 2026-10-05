import fs from "node:fs";
import path from "node:path";
import { parseDocument } from "yaml";

// The layout of a workspace: one directory per hook or policy, holding
// hook.yaml or policy.yaml, the script and its tests.

export const KINDS = {
  policy: { file: "policy.yaml", kind: "JSAdmission", dir: "policies", resource: "jsadmission" },
  hook: { file: "hook.yaml", kind: "JSHook", dir: "hooks", resource: "jshook" },
} as const;
export type Kind = keyof typeof KINDS;

const SKIP = new Set(["node_modules", "dist"]);

/** A hook or policy of the workspace. */
export interface Entry {
  kind: Kind;
  /** metadata.name */
  name: string;
  dir: string;
  manifest: string;
}

/** Reads the hook or policy of a directory; undefined when it has none. */
export function entryIn(dir: string): Entry | undefined {
  for (const kind of Object.keys(KINDS) as Kind[]) {
    const manifest = path.join(dir, KINDS[kind].file);
    if (!fs.existsSync(manifest)) continue;
    const doc = parseDocument(fs.readFileSync(manifest, "utf8"));
    const name = doc.getIn(["metadata", "name"]);
    return { kind, name: typeof name === "string" && name ? name : path.basename(dir), dir, manifest };
  }
  return undefined;
}

/** Every hook and policy below root, in a stable order. */
export function entries(root = "."): Entry[] {
  const found: Entry[] = [];
  const walk = (dir: string) => {
    const e = entryIn(dir);
    if (e) found.push(e);
    for (const d of fs.readdirSync(dir, { withFileTypes: true })) {
      if (d.isDirectory() && !SKIP.has(d.name) && !d.name.startsWith(".")) walk(path.join(dir, d.name));
    }
  };
  walk(root);
  return found.sort((a, b) => a.dir.localeCompare(b.dir));
}

/** The one hook or policy a name or directory names; throws otherwise. */
export function find(nameOrDir: string): Entry {
  if (fs.existsSync(nameOrDir) && fs.statSync(nameOrDir).isDirectory()) {
    const e = entryIn(nameOrDir);
    if (e) return e;
  }
  const hits = entries().filter((e) => e.name === nameOrDir || path.basename(e.dir) === nameOrDir);
  if (hits.length === 1) return hits[0];
  if (hits.length === 0) throw new Error(`no hook or policy named ${nameOrDir}`);
  throw new Error(`${nameOrDir} names ${hits.map((h) => h.dir).join(" and ")}; give the directory instead`);
}

// metadata.name becomes part of ServiceAccount and webhook names, so a
// DNS label keeps every one of them valid.
const NAME = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;

export function checkName(name: string): void {
  if (!NAME.test(name)) {
    throw new Error(`"${name}" is not a valid name: use lowercase letters, digits and "-", at most 63 characters`);
  }
}
