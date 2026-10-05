import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";
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

interface Pending {
  resolve: (a: Answer) => void;
  reject: (e: Error) => void;
}

/**
 * One `gojsop serve --stdio` child per Node process, started on the first
 * call. Requests carry an id, so calls may overlap.
 */
class Server {
  private child?: ChildProcess;
  private nextId = 1;
  private pending = new Map<number, Pending>();

  call(request: Record<string, unknown>): Promise<Answer> {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.start().stdin!.write(JSON.stringify({ ...request, id }) + "\n", (err) => {
        if (err) {
          this.pending.delete(id);
          reject(err);
        }
      });
    });
  }

  private start(): ChildProcess {
    if (this.child) return this.child;
    const bin = process.env.GOJSOP_BIN || "gojsop";
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
      end(
        e.code === "ENOENT"
          ? `cannot start the gojsop binary "${bin}": not found. Install gojsop and put it on PATH, or set GOJSOP_BIN to its path.`
          : `cannot start the gojsop binary "${bin}": ${e.message}`,
      ),
    );
    child.on("exit", (code, signal) => end(`gojsop serve ended unexpectedly (${signal ?? "exit " + code})`));
    child.stdin!.on("error", () => {}); // reported through "exit" or the write callback

    createInterface({ input: child.stdout! }).on("line", (line) => {
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

/** Sends one call to the serve process of this Node process. */
export function callServe(
  op: "review" | "handle",
  manifest: string,
  input: unknown,
  cluster: KubeObject[],
): Promise<Answer> {
  server ??= new Server();
  return server.call({ op, manifest, input, cluster });
}

/** Throws a ScriptError for an error answer. */
export function raise(a: Answer): void {
  if (a.error) throw new ScriptError(a.error.kind, a.error.message);
}
