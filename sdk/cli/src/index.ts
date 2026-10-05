import { createRequire } from "node:module";
import { platforms } from "./platforms.js";

const require = createRequire(import.meta.url);

/**
 * The path of the gojsop binary: GOJSOP_BIN if set, else the binary of the
 * platform package npm installed, else "gojsop" from PATH.
 */
export function binaryPath(): string {
  if (process.env.GOJSOP_BIN) return process.env.GOJSOP_BIN;
  const p = platforms[`${process.platform}-${process.arch}`];
  if (p) {
    const exe = process.platform === "win32" ? "gojsop.exe" : "gojsop";
    try {
      return require.resolve(`${p.pkg}/bin/${exe}`);
    } catch {
      // not installed (unsupported platform, --omit=optional); fall back to PATH
    }
  }
  return "gojsop";
}

/** Explains how to get the binary when binaryPath() points nowhere. */
export function notFoundHint(bin: string): string {
  return (
    `cannot start the gojsop binary "${bin}": not found. ` +
    `Reinstall @gojsop/cli without --omit=optional, put gojsop on PATH, or set GOJSOP_BIN to its path.`
  );
}
