import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, expect, test } from "vitest";
import { parse } from "yaml";
import { build } from "../src/build.js";
import { create, remove, rename } from "../src/scaffold.js";

const pod = path.resolve(import.meta.dirname, "../../create/template/policies/no-latest/pod.yaml");
let cwd: string;

beforeEach(() => {
  cwd = process.cwd();
  process.chdir(fs.mkdtempSync(path.join(os.tmpdir(), "gojsop-ws-")));
});
afterEach(() => process.chdir(cwd));

// workspace.R12
test("new, rn and rm keep the name in one place", async () => {
  create("policy", "require-owner");
  expect(fs.readdirSync("policies/require-owner").sort()).toEqual(["policy.test.ts", "policy.ts", "policy.yaml"]);
  expect(() => create("policy", "require-owner")).toThrow(/exists/);
  expect(() => create("policy", "Bad_Name")).toThrow(/not a valid name/);

  rename("require-owner", "need-owner");
  expect(fs.existsSync("policies/require-owner")).toBe(false);
  expect(parse(fs.readFileSync("policies/need-owner/policy.yaml", "utf8")).metadata.name).toBe("need-owner");

  create("hook", "need-owner");
  expect(() => rename("need-owner", "x")).toThrow(/give the directory/);
  await remove("hooks/need-owner", true);
  expect(fs.existsSync("hooks/need-owner")).toBe(false);
  await expect(remove("need-owner", false)).rejects.toThrow(/not removed/); // no TTY, no --yes
});

// workspace.R13
test("build writes one manifest per hook and policy, script inline, and drops stale ones", async () => {
  create("policy", "no-latest");
  fs.writeFileSync(
    "policies/no-latest/policy.ts",
    `export function validate(req: { object?: { spec?: { containers?: { image?: string }[] } } }) {
  const latest = (req.object?.spec?.containers ?? []).some((c) => c.image?.endsWith(":latest"));
  return latest ? { allowed: false, message: "uses :latest" } : { allowed: true };
}
`,
  );
  create("hook", "old");
  await build();
  expect(fs.readdirSync("dist").sort()).toEqual(["hook-old.yaml", "policy-no-latest.yaml"]);
  const built = parse(fs.readFileSync("dist/policy-no-latest.yaml", "utf8"));
  expect(built.spec.source.inline).toMatch(/globalThis\.validate/);

  await remove("old", true);
  await build();
  expect(fs.readdirSync("dist")).toEqual(["policy-no-latest.yaml"]);

  // The built manifest runs as it is in the operator's engine.
  const bin = process.env.GOJSOP_BIN;
  if (bin) {
    const r = spawnSync(bin, ["run", "dist/policy-no-latest.yaml", "--request", pod], { encoding: "utf8" });
    expect(JSON.parse(r.stdout)).toMatchObject({ allowed: false, message: "uses :latest" });
  }
});
