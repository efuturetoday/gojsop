import fs from "node:fs";
import path from "node:path";
import { parseDocument, Scalar } from "yaml";
import { bundle, isModule, scriptBeside } from "./bundle.js";
import { entries } from "./workspace.js";

/** The most spec.source.inline may hold. */
const INLINE_LIMIT = 512 * 1024;
const OWN = /^(policy|hook)-.+\.yaml$/;

/**
 * gojsop build: writes every hook and policy of the workspace to
 * dist/<policy|hook>-<name>.yaml, with its script bundled into
 * spec.source.inline, ready for kubectl apply -f dist/. Files it wrote
 * before for hooks and policies that are gone are removed.
 */
export async function build(out = "dist"): Promise<number> {
  const all = entries();
  fs.mkdirSync(out, { recursive: true });
  for (const f of fs.readdirSync(out)) if (OWN.test(f)) fs.rmSync(path.join(out, f));

  const seen = new Set<string>();
  for (const e of all) {
    const file = `${e.kind}-${e.name}.yaml`;
    if (seen.has(file)) throw new Error(`two ${e.kind === "hook" ? "hooks" : "policies"} are named ${e.name}`);
    seen.add(file);

    const doc = parseDocument(fs.readFileSync(e.manifest, "utf8"));
    if (!doc.hasIn(["spec", "source", "inline"]) && !doc.hasIn(["spec", "source", "configMapRef"])) {
      const entry = scriptBeside(e.dir);
      if (!entry) throw new Error(`${e.dir}: no script next to ${path.basename(e.manifest)}`);
      const code = isModule(entry) ? (await bundle(entry)).code : fs.readFileSync(entry, "utf8");
      if (Buffer.byteLength(code) > INLINE_LIMIT) {
        throw new Error(`${entry}: the bundled script has ${Buffer.byteLength(code)} bytes, more than 512 KiB`);
      }
      const inline = new Scalar(code);
      inline.type = Scalar.BLOCK_LITERAL;
      doc.setIn(["spec", "source"], doc.createNode({ inline }));
    }
    fs.writeFileSync(path.join(out, file), doc.toString({ lineWidth: 0, flowCollectionPadding: false }));
    console.log(`built ${path.join(out, file)}`);
  }
  if (all.length === 0) console.log("no hooks or policies found");
  return all.length;
}
