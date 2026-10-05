import fs from "node:fs";
import path from "node:path";
import { createInterface } from "node:readline/promises";
import { parseDocument } from "yaml";
import { checkName, find, KINDS, type Kind } from "./workspace.js";

// What gojsop new writes: a manifest, a script and a test that passes.
const FILES: Record<Kind, (name: string) => Record<string, string>> = {
  policy: (name) => ({
    "policy.yaml": `apiVersion: core.gojsop.io/v1alpha1
kind: JSAdmission
metadata:
  name: ${name}
spec:
  type: validating
  rules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["pods"]
      operations: ["CREATE", "UPDATE"]
  # Audit records what the policy would deny and blocks nothing; switch to
  # Deny once kubectl describe jsadmission ${name} shows no surprises.
  enforcement: Audit
`,
    "policy.ts": `import type { Request, Response } from "@gojsop/types";

export function validate(req: Request): Response {
  return { allowed: true };
}
`,
    "policy.test.ts": `import { expect, test } from "vitest";
import { pod, policy } from "@gojsop/testing";

const ${camel(name)} = policy("./policy.yaml");

test("allows a pod", async () => {
  const result = await ${camel(name)}.review({ object: pod() });
  expect(result.allowed).toBe(true);
});
`,
  }),
  hook: (name) => ({
    "hook.yaml": `apiVersion: core.gojsop.io/v1alpha1
kind: JSHook
metadata:
  name: ${name}
spec:
  bindings:
    - name: pods
      apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["pods"]
  # What kube.* may do besides reading the bindings, for example:
  # permissions:
  #   - apiGroups: [""]
  #     resources: ["configmaps"]
  #     verbs: ["get", "create", "patch"]
`,
    "hook.ts": `import type { Event } from "@gojsop/types";

export function handle(event: Event) {
  console.log(event.type, event.object.metadata?.name);
}
`,
    "hook.test.ts": `import { expect, test } from "vitest";
import { hook, pod } from "@gojsop/testing";

const ${camel(name)} = hook("./hook.yaml");

test("logs the pod", async () => {
  const result = await ${camel(name)}.handle({ object: pod({ name: "web" }) });
  expect(result.console.map((l) => l.text)).toEqual(["Added web"]);
});
`,
  }),
};

function camel(name: string): string {
  return name.replace(/-([a-z0-9])/g, (_, c: string) => c.toUpperCase());
}

/** gojsop new <policy|hook> <name>: a new directory with a passing test. */
export function create(kind: string, name: string): void {
  if (kind !== "policy" && kind !== "hook") throw new Error(`gojsop new: give policy or hook, not "${kind}"`);
  checkName(name);
  const dir = path.join(KINDS[kind].dir, name);
  if (fs.existsSync(dir)) throw new Error(`${dir} exists already`);
  fs.mkdirSync(dir, { recursive: true });
  for (const [file, text] of Object.entries(FILES[kind](name))) fs.writeFileSync(path.join(dir, file), text);
  console.log(`created ${dir}: ${Object.keys(FILES[kind](name)).join(", ")}`);
}

async function confirm(question: string): Promise<boolean> {
  if (!process.stdin.isTTY) return false;
  const rl = createInterface({ input: process.stdin, output: process.stdout });
  const answer = await rl.question(`${question} [y/N] `);
  rl.close();
  return /^y(es)?$/i.test(answer.trim());
}

function stillDeployed(kind: Kind, name: string): string {
  return `If it is deployed, it stays in the cluster until: kubectl delete ${KINDS[kind].resource} ${name}`;
}

/** gojsop rm <name>: removes the directory of a hook or policy. */
export async function remove(nameOrDir: string, yes: boolean): Promise<void> {
  const e = find(nameOrDir);
  if (!yes && !(await confirm(`Remove ${e.dir}?`))) {
    throw new Error(`not removed; confirm, or pass --yes`);
  }
  fs.rmSync(e.dir, { recursive: true });
  console.log(`removed ${e.dir}\n${stillDeployed(e.kind, e.name)}`);
}

/** gojsop rn <name> <new-name>: renames a hook or policy and its directory. */
export function rename(nameOrDir: string, to: string): void {
  checkName(to);
  const e = find(nameOrDir);
  const doc = parseDocument(fs.readFileSync(e.manifest, "utf8"));
  doc.setIn(["metadata", "name"], to);
  fs.writeFileSync(e.manifest, doc.toString({ lineWidth: 0, flowCollectionPadding: false }));
  let dir = e.dir;
  if (path.basename(e.dir) === e.name) {
    dir = path.join(path.dirname(e.dir), to);
    if (fs.existsSync(dir)) throw new Error(`${dir} exists already`);
    fs.renameSync(e.dir, dir);
  }
  console.log(`renamed ${e.name} to ${to} (${dir})\n${stillDeployed(e.kind, e.name)}`);
}
