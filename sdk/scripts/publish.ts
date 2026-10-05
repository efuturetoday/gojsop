// Publishes the npm packages of one release:
//
//   node scripts/publish.ts --version 0.2.0 --bin-dir ../dist/cli [--dry-run] [--pack <dir>]
//
// The release tag is the only source of the version: this script stamps it
// into every package and pins the packages to each other. It builds one
// package per platform around the binaries of `make cli-dist`, then
// publishes the platforms, @gojsop/cli, @gojsop/types, @gojsop/testing and
// @gojsop/create,
// in that order. A version that is already on npm is skipped, so a failed
// release job can be rerun. --pack writes tarballs instead of publishing.
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { platforms } from "../cli/src/platforms.ts";

const { values: args } = parseArgs({
  options: {
    version: { type: "string" },
    "bin-dir": { type: "string" },
    "dry-run": { type: "boolean", default: false },
    pack: { type: "string" },
  },
});
const version = args.version?.replace(/^v/, "");
if (!version || !args["bin-dir"]) {
  console.error("usage: publish.ts --version <x.y.z> --bin-dir <dir> [--dry-run] [--pack <dir>]");
  process.exit(2);
}

const sdk = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const binDir = path.resolve(args["bin-dir"]);
const build = path.join(sdk, "build");
fs.rmSync(build, { recursive: true, force: true });

// A package.json: the fields this script reads and writes.
interface Manifest {
  name: string;
  version: string;
  license?: string;
  repository?: unknown;
  dependencies?: Record<string, string>;
  optionalDependencies?: Record<string, string>;
}

const readJSON = (f: string): Manifest => JSON.parse(fs.readFileSync(f, "utf8"));
const writeJSON = (f: string, v: unknown) => fs.writeFileSync(f, `${JSON.stringify(v, null, 2)}\n`);
const cli = readJSON(path.join(sdk, "cli/package.json"));

// One package per platform, holding only the binary.
const dirs: string[] = [];
for (const [key, p] of Object.entries(platforms)) {
  const [os, cpu] = key.split("-");
  const exe = p.goos === "windows" ? "gojsop.exe" : "gojsop";
  const src = path.join(binDir, `gojsop_${version}_${p.goos}_${p.goarch}${p.goos === "windows" ? ".exe" : ""}`);
  if (!fs.existsSync(src)) throw new Error(`missing binary ${src}; run make cli-dist VERSION=${version}`);
  const dir = path.join(build, key);
  fs.mkdirSync(path.join(dir, "bin"), { recursive: true });
  fs.copyFileSync(src, path.join(dir, "bin", exe));
  fs.chmodSync(path.join(dir, "bin", exe), 0o755);
  writeJSON(path.join(dir, "package.json"), {
    name: p.pkg,
    version,
    description: `The gojsop binary for ${key}; installed by @gojsop/cli.`,
    license: cli.license,
    repository: cli.repository,
    os: [os],
    cpu: [cpu],
    files: ["bin"],
  });
  fs.writeFileSync(
    path.join(dir, "README.md"),
    `# ${p.pkg}\n\nThe gojsop binary for ${key}. Install [@gojsop/cli](https://www.npmjs.com/package/@gojsop/cli) instead.\n`,
  );
  dirs.push(dir);
}

// Stamp the version and pin the packages to each other; the manifests are
// restored at the end, so a local --pack leaves the repository unchanged.
const originals = new Map<string, string>();
const stamp = (name: string, edit?: (pkg: Manifest) => void) => {
  const f = path.join(sdk, name, "package.json");
  if (!originals.has(f)) originals.set(f, fs.readFileSync(f, "utf8"));
  const pkg = readJSON(f);
  pkg.version = version;
  edit?.(pkg);
  writeJSON(f, pkg);
  dirs.push(path.join(sdk, name));
};
stamp("cli", (pkg) => {
  pkg.optionalDependencies = Object.fromEntries(Object.values(platforms).map((p) => [p.pkg, version]));
});
stamp("types");
stamp("testing", (pkg) => {
  pkg.dependencies = { ...pkg.dependencies, "@gojsop/cli": version };
});
stamp("create");
for (const name of ["cli", "testing", "create"]) {
  if (!fs.existsSync(path.join(sdk, name, "dist"))) throw new Error(`${name}/dist is missing; run npm run build first`);
}

const npm = (argv: string[], cwd: string) => execFileSync("npm", argv, { cwd, stdio: "inherit" });
const published = (name: string) => {
  try {
    execFileSync("npm", ["view", `${name}@${version}`, "version"], { stdio: "pipe" });
    return true;
  } catch {
    return false;
  }
};

try {
  for (const dir of dirs) {
    const { name } = readJSON(path.join(dir, "package.json"));
    if (args.pack) {
      fs.mkdirSync(args.pack, { recursive: true });
      npm(["pack", "--pack-destination", path.resolve(args.pack)], dir);
    } else if (!args["dry-run"] && published(name)) {
      console.log(`${name}@${version} is on npm already; skipped`);
    } else {
      const flags = ["publish", "--access", "public"];
      if (args["dry-run"]) flags.push("--dry-run");
      else if (process.env.GITHUB_ACTIONS) flags.push("--provenance");
      npm(flags, dir);
    }
  }
} finally {
  for (const [f, text] of originals) fs.writeFileSync(f, text);
}
