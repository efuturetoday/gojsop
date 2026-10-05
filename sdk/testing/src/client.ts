import { type ChildProcessByStdio, spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { createInterface } from "node:readline";
import type { Readable, Writable } from "node:stream";
import { type Bundle, binaryPath, bundle, isModule, notFoundHint, scriptBeside } from "@gojsop/cli";
import { SourceMapConsumer } from "source-map-js";
import type { ConsoleLine, ErrorKind, KubeObject } from "./types.js";

/** A call failed: the script threw, ran out of time or memory, or the input was bad. */
export class ScriptError extends Error {
  constructor(
    readonly kind: ErrorKind,
    message: string,
  ) {
    super(message);
    this.name = "ScriptError";
  }
}

/** One answer line of `gojsop serve --stdio`. */
export interface Answer {
  id: number;
  result?: Record<string, unknown>;
  console: ConsoleLine[];
  cluster: KubeObject[];
  error?: { kind: ErrorKind; message: string };
}

/** The serve child: stdin and stdout are pipes. */
type Child = ChildProcessByStdio<Writable, Readable, null>;

interface Pending {
  resolve: (a: Answer) => void;
  reject: (e: Error) => void;
}

/**
 * One `gojsop serve --stdio` child per Node process, started on the first
 * call. Requests carry an id, so calls may overlap.
 */
class Server {
  private child?: Child;
  private nextId = 1;
  private pending = new Map<number, Pending>();

  call(request: Record<string, unknown>): Promise<Answer> {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.start().stdin.write(`${JSON.stringify({ ...request, id })}\n`, (err) => {
        if (err) {
          this.pending.delete(id);
          reject(err);
        }
      });
    });
  }

  private start(): Child {
    if (this.child) return this.child;
    const bin = binaryPath();
    const child = spawn(bin, ["serve", "--stdio"], { stdio: ["pipe", "pipe", "inherit"] });
    this.child = child;
    // The child must not keep Node alive; it ends with Node (or on EOF of stdin).
    child.unref();
    (child.stdin as unknown as { unref?: () => void }).unref?.();
    (child.stdout as unknown as { unref?: () => void }).unref?.();
    process.on("exit", () => child.kill());

    const end = (why: string) => {
      if (this.child === child) this.child = undefined; // the next call starts a new one
      const err = new Error(why);
      for (const p of this.pending.values()) p.reject(err);
      this.pending.clear();
    };
    child.on("error", (e: NodeJS.ErrnoException) =>
      end(e.code === "ENOENT" ? notFoundHint(bin) : `cannot start the gojsop binary "${bin}": ${e.message}`),
    );
    child.on("exit", (code, signal) => end(`gojsop serve ended unexpectedly (${signal ?? `exit ${code}`})`));
    child.stdin.on("error", () => {}); // reported through "exit" or the write callback

    createInterface({ input: child.stdout }).on("line", (line) => {
      let a: Answer;
      try {
        a = JSON.parse(line);
      } catch {
        return; // serve writes JSON lines only; ignore anything else
      }
      const p = this.pending.get(a.id);
      this.pending.delete(a.id);
      p?.resolve(a);
    });
    return child;
  }
}

let server: Server | undefined;

// Bundles by entry file. A bundle is reused while none of its files
// changed, so watch mode sees edits without a rebuild per call.
const bundles = new Map<string, { bundle: Bundle; mtimes: number[] }>();

function mtimes(files: string[]): number[] {
  return files.map((f) => {
    try {
      return fs.statSync(f).mtimeMs;
    } catch {
      return -1;
    }
  });
}

/**
 * The script to send with a call: a TypeScript or module script next to the
 * manifest, bundled; undefined for a plain .js script or an inline one,
 * which gojsop reads itself.
 */
async function sourceFor(manifest: string): Promise<Bundle | undefined> {
  const st = fs.statSync(manifest, { throwIfNoEntry: false });
  if (!st) return undefined; // gojsop reports the missing manifest
  const dir = st.isDirectory() ? manifest : path.dirname(manifest);
  const entry = scriptBeside(dir);
  if (!entry || !isModule(entry)) return undefined;
  const cached = bundles.get(entry);
  if (cached && mtimes(cached.bundle.files).every((m, i) => m === cached.mtimes[i])) {
    return cached.bundle;
  }
  const b = await bundle(entry);
  bundles.set(entry, { bundle: b, mtimes: mtimes(b.files) });
  return b;
}

// Points the stack frames of a bundled script's error, "(name.js:4:28)",
// at the TypeScript lines they came from, so vitest shows those.
function mapStack(message: string, b: Bundle): string {
  if (!b.map) return message;
  const consumer = new SourceMapConsumer({ ...b.map, version: String(b.map.version) });
  return message.replace(/\(([^():\s]+\.js):(\d+):(\d+)\)/g, (frame, _file, line, col) => {
    const pos = consumer.originalPositionFor({ line: Number(line), column: Math.max(0, Number(col) - 1) });
    if (!pos.source || pos.line == null) return frame;
    return `(${pos.source}:${pos.line}:${(pos.column ?? 0) + 1})`;
  });
}

/** Sends one call to the serve process of this Node process. */
export async function callServe(
  op: "review" | "handle",
  manifest: string,
  input: unknown,
  cluster: KubeObject[],
): Promise<Answer> {
  let built: Awaited<ReturnType<typeof sourceFor>>;
  try {
    built = await sourceFor(manifest);
  } catch (e) {
    throw new ScriptError("script", e instanceof Error ? e.message : String(e));
  }
  server ??= new Server();
  const a = await server.call({
    op,
    manifest,
    // A string is the path of a YAML or JSON file; gojsop reads it.
    ...(typeof input === "string" ? { inputFile: input } : { input }),
    cluster,
    ...(built ? { source: built.code } : {}),
  });
  if (a.error && built) a.error.message = mapStack(a.error.message, built);
  return a;
}

/** Throws a ScriptError for an error answer. */
export function raise(a: Answer): void {
  if (a.error) throw new ScriptError(a.error.kind, a.error.message);
}
