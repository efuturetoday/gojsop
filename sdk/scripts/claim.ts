// Sets up the npm packages for trusted publishing, once, by a maintainer:
//
//   npm login && make sdk-claim
//
// For every package it publishes a placeholder 0.0.0 if the package does
// not exist yet (npm accepts trusted publishing only for existing
// packages), then trusts .github/workflows/release.yml of this repository
// to publish it. Run it in a terminal: npm asks for 2FA.
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { platforms } from "../cli/src/platforms.ts";

const names = [
  ...Object.values(platforms).map((p) => p.pkg),
  "@gojsop/cli",
  "@gojsop/types",
  "@gojsop/testing",
  "@gojsop/create",
];

const exists = (name: string) => {
  try {
    execFileSync("npm", ["view", name, "name"], { stdio: "pipe" });
    return true;
  } catch {
    return false;
  }
};

if (!process.stdin.isTTY) {
  console.error("Run this in a terminal: npm asks for 2FA.");
  process.exit(1);
}

for (const name of names) {
  if (exists(name)) {
    console.log(`${name} exists; skipped`);
    continue;
  }
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "gojsop-claim-"));
  fs.writeFileSync(
    path.join(dir, "package.json"),
    `${JSON.stringify(
      {
        name,
        version: "0.0.0",
        description: "Placeholder; the gojsop release job publishes the real versions.",
        license: "Apache-2.0",
        repository: { type: "git", url: "git+https://github.com/efuturetoday/gojsop.git", directory: "sdk" },
      },
      null,
      2,
    )}\n`,
  );
  fs.writeFileSync(
    path.join(dir, "README.md"),
    `# ${name}\n\nPart of [gojsop](https://github.com/efuturetoday/gojsop). This 0.0.0 is a placeholder.\n`,
  );
  // stdio inherit: npm asks for the 2FA code here.
  execFileSync("npm", ["publish", "--access", "public"], { cwd: dir, stdio: "inherit" });
}

for (const name of names) {
  const trusted = execFileSync("npm", ["trust", "list", name, "--json"], { encoding: "utf8" });
  if (trusted.includes("release.yml")) {
    console.log(`${name} trusts release.yml already; skipped`);
    continue;
  }
  execFileSync(
    "npm",
    ["trust", "github", name, "--repo", "efuturetoday/gojsop", "--file", "release.yml", "--allow-publish", "--yes"],
    { stdio: "inherit" },
  );
}
