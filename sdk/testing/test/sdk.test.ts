import { mkdtempSync, utimesSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { cluster, configMap, hook, namespace, pod, policy, ScriptError, secret } from "@gojsop/testing";
import { describe, expect, test } from "vitest";

const policyHeader = `apiVersion: core.gojsop.io/v1alpha1
kind: JSAdmission
metadata: {name: p}
spec:
  rules: [{apiGroups: [""], apiVersions: [v1], resources: [pods], operations: [CREATE]}]
`;
const hookHeader = `apiVersion: core.gojsop.io/v1alpha1
kind: JSHook
metadata: {name: h}
spec:
  bindings: [{name: pods, apiGroups: [""], apiVersions: [v1], resources: [pods]}]
`;

// A manifest in a temporary directory; returns its path.
function manifest(file: "policy.yaml" | "hook.yaml", yaml: string): string {
  const dir = mkdtempSync(path.join(tmpdir(), "gojsop-sdk-"));
  writeFileSync(path.join(dir, file), yaml);
  return path.join(dir, file);
}

describe("errors", () => {
  const p = policy(
    manifest(
      "policy.yaml",
      policyHeader +
        `  limits: {memoryMB: 4, timeoutSeconds: 1}
  source:
    inline: |
      function validate(req) {
        const mode = req.object.metadata.name;
        if (mode === "throw") throw new Error("boom");
        if (mode === "loop") for (;;) {}
        if (mode === "memory") { const a = []; for (;;) a.push(new Array(100000).fill(1)); }
        return {allowed: true};
      }
`,
    ),
  );

  test("a throwing script rejects with kind script", async () => {
    const err = await p.review({ object: pod({ name: "throw" }) }).catch((e) => e);
    expect(err).toBeInstanceOf(ScriptError);
    expect(err.kind).toBe("script");
    expect(err.message).toMatch(/boom/);
  });

  test("a script over its time limit rejects with timeout", async () => {
    await expect(p.review({ object: pod({ name: "loop" }) })).rejects.toThrow(/timeout/);
    await expect(p.review({ object: pod({ name: "loop" }) })).rejects.toMatchObject({ kind: "timeout" });
  });

  test("a script over its memory limit rejects with memoryLimit", async () => {
    await expect(p.review({ object: pod({ name: "memory" }) })).rejects.toMatchObject({ kind: "memoryLimit" });
  });

  test("the server keeps answering after every failure", async () => {
    const r = await p.review({ object: pod({ name: "fine" }) });
    expect(r.allowed).toBe(true);
  });

  test("a bad manifest path or input rejects with kind input", async () => {
    await expect(policy("./nope/policy.yaml").review({ object: pod() })).rejects.toMatchObject({ kind: "input" });
    await expect(p.review({})).rejects.toMatchObject({ kind: "input" });
  });
});

describe("kube access", () => {
  const h = hook(
    manifest(
      "hook.yaml",
      hookHeader +
        `  permissions: [{apiGroups: [""], resources: [configmaps], verbs: [get, create]}]
  source:
    inline: |
      function handle(event) {
        kube.get({apiVersion: "v1", kind: "Secret", namespace: "default", name: "s"});
      }
`,
    ),
  );

  test("kube.* without the right in spec.permissions throws Forbidden", async () => {
    const err = await h.handle({ object: pod() }, { cluster: [secret("default/s")] }).catch((e) => e);
    expect(err).toBeInstanceOf(ScriptError);
    expect(err.message).toMatch(/forbidden/i);
  });
});

describe("cluster", () => {
  test("add, get, list and objects", () => {
    const c = cluster([namespace("shop")]);
    c.add(configMap("shop/a", { data: { k: "v" } })).add(configMap("other/b"));
    c.add(configMap("shop/a", { data: { k: "w" } })); // replaces
    expect(c.get("v1", "ConfigMap", "shop", "a")?.data).toEqual({ k: "w" });
    expect(c.get("v1", "ConfigMap", "shop", "missing")).toBeUndefined();
    expect(c.get("v1", "Namespace", "", "shop")).toBeDefined();
    expect(c.list("v1", "ConfigMap")).toHaveLength(2);
    expect(c.list("v1", "ConfigMap", "shop")).toHaveLength(1);
    expect(c.objects()).toHaveLength(3);
  });

  test("a hook's writes show in the cluster after the call", async () => {
    const h = hook(
      manifest(
        "hook.yaml",
        hookHeader +
          `  permissions: [{apiGroups: [""], resources: [configmaps], verbs: [get, create, patch, delete]}]
  source:
    inline: |
      function handle(event) {
        kube.apply({apiVersion: "v1", kind: "ConfigMap", metadata: {name: "seen", namespace: "default"}, data: {pod: event.object.metadata.name}});
        kube.delete({apiVersion: "v1", kind: "ConfigMap", namespace: "default", name: "old"});
      }
`,
      ),
    );
    const c = cluster([configMap("default/old")]);
    await h.handle({ object: pod({ name: "a" }) }, { cluster: c });
    expect(c.get("v1", "ConfigMap", "default", "seen")?.data).toEqual({ pod: "a" });
    expect(c.get("v1", "ConfigMap", "default", "old")).toBeUndefined();
  });
});

describe("fixtures", () => {
  test("have sensible defaults", () => {
    expect(pod()).toMatchObject({ kind: "Pod", metadata: { name: "pod", namespace: "default" } });
    expect(pod({ image: "x:1", labels: { a: "b" } }).metadata?.labels).toEqual({ a: "b" });
    expect(configMap("ns/n", { data: { a: "b" } })).toMatchObject({
      metadata: { namespace: "ns", name: "n" },
      data: { a: "b" },
    });
    expect(namespace("n").metadata).toEqual({ name: "n" });
  });
});

describe("TypeScript", () => {
  const ts = policy("./ts-policy/policy.yaml");

  test("bundles policy.ts with its imports and runs it in the engine", async () => {
    const result = await ts.review({ object: pod({ image: "nginx:latest" }) });
    expect(result).toMatchObject({ allowed: false, message: "nginx:latest uses :latest" });
  });

  test("points a script error at the TypeScript line", async () => {
    const err = await ts.review({ object: pod({ image: "boom:1" }) }).catch((e) => e);
    expect(err).toBeInstanceOf(ScriptError);
    expect(err.kind).toBe("script");
    expect(err.message).toMatch(/refused boom:1/);
    expect(err.message).toMatch(/ts-policy[\\/]rules\.ts:4:\d+\)/);
  });

  test("rebuilds after the script changes", async () => {
    const file = manifest("policy.yaml", policyHeader);
    const script = path.join(path.dirname(file), "policy.ts");
    writeFileSync(script, `export function validate() { return { allowed: true }; }`);
    expect((await policy(file).review({ object: pod() })).allowed).toBe(true);
    // a later mtime, as an editor's save gives
    writeFileSync(script, `export function validate() { return { allowed: false, message: "changed" }; }`);
    utimesSync(script, new Date(), new Date(Date.now() + 5000));
    expect((await policy(file).review({ object: pod() })).message).toBe("changed");
  });
});
