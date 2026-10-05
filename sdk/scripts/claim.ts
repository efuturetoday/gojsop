// Publishes a placeholder 0.0.0 of every npm package that does not exist yet:
//
//   npm login && node scripts/claim.ts
//
// Run once, by a maintainer, before the first release. npm accepts trusted
// publishing (OIDC from release.yml) only for packages that exist, so the
// names have to be claimed by hand first; then add the trusted publisher
// to each package on npmjs.com (see CONTRIBUTING.md).
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
