#!/usr/bin/env node
// npm create @gojsop [dir]: copies the template into dir (default
// "gojsop-workspace") and pins the @gojsop packages to this package's version.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// dist/index.js; the template and package.json sit one level up.
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const { version } = JSON.parse(fs.readFileSync(path.join(root, "package.json"), "utf8")) as { version: string };

const dir = process.argv[2] ?? "gojsop-workspace";
const target = path.resolve(dir);
if (fs.existsSync(target) && fs.readdirSync(target).length > 0) {
  console.error(`${dir} exists and is not empty; choose another directory.`);
  process.exit(1);
}

fs.cpSync(path.join(root, "template"), target, { recursive: true });
// npm drops .gitignore from published packages, so the template carries _gitignore.
fs.renameSync(path.join(target, "_gitignore"), path.join(target, ".gitignore"));

const pkgPath = path.join(target, "package.json");
const pkg = JSON.parse(fs.readFileSync(pkgPath, "utf8"));
pkg.name = path.basename(target);
for (const dep of Object.keys(pkg.devDependencies)) {
  if (dep.startsWith("@gojsop/")) pkg.devDependencies[dep] = `^${version}`;
}
fs.writeFileSync(pkgPath, `${JSON.stringify(pkg, null, 2)}\n`);

console.log(`Created ${dir}: one policy (policies/no-latest) and one hook (hooks/count-pods), in TypeScript, with tests.

  cd ${dir}
  npm install
  npm test
`);
