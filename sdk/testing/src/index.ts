import path from "node:path";
import { callServe, raise } from "./client.js";
import { Cluster } from "./cluster.js";
import type { AdmissionRequest, HandleResult, HookEvent, KubeObject, ReviewResult } from "./types.js";

export { ScriptError } from "./client.js";
export { Cluster, cluster } from "./cluster.js";
export { configMap, deployment, namespace, pod, secret, service } from "./fixtures.js";
export type {
  AdmissionRequest,
  ConsoleLine,
  ErrorKind,
  HandleResult,
  HookEvent,
  KubeObject,
  ReviewResult,
} from "./types.js";

/** The fake cluster of a call: a `cluster()` (updated after the call) or a list of objects. */
export interface CallOptions {
  cluster?: Cluster | KubeObject[];
}

// Relative manifest paths are relative to the test file that calls policy()
// or hook(); outside vitest, to the working directory.
let testPath: (() => string | undefined) | undefined;
try {
  const v = await import("vitest");
  testPath = () => v.expect.getState().testPath;
} catch {
  // not under vitest
}

function resolvePath(p: string): string {
  if (path.isAbsolute(p)) return p;
  let base = process.cwd();
  try {
    const t = testPath?.();
    if (t) base = path.dirname(t);
  } catch {
    // no test running
  }
  return path.resolve(base, p);
}

async function run<R>(op: "review" | "handle", manifest: string, input: unknown, opts?: CallOptions): Promise<R> {
  const c = opts?.cluster;
  const objects = c instanceof Cluster ? c.objects() : (c ?? []);
  const a = await callServe(op, manifest, typeof input === "string" ? resolvePath(input) : input, objects);
  if (c instanceof Cluster) c.replace(a.cluster);
  raise(a);
  return { ...a.result, console: a.console } as R;
}

/**
 * A policy (`JSAdmission`) to test; `manifestPath` is the manifest or its
 * directory. The script runs in gojsop's engine, never in Node.
 */
export function policy(manifestPath: string) {
  return {
    /**
     * Runs `validate` or `mutate` on the request: an object, or the path of a
     * YAML file (relative to the test file) holding a request or a plain
     * object such as `kubectl get pod web -o yaml` prints. Rejects with a
     * ScriptError when the script fails.
     */
    review(request: AdmissionRequest | string, opts?: CallOptions): Promise<ReviewResult> {
      return run("review", resolvePath(manifestPath), request, opts);
    },
  };
}

/**
 * A hook (`JSHook`) to test; `manifestPath` is the manifest or its
 * directory. The script runs in gojsop's engine, never in Node.
 */
export function hook(manifestPath: string) {
  return {
    /**
     * Runs `handle` on the event: an object, or the path of a YAML file
     * (relative to the test file) holding an event or a plain object.
     * Rejects with a ScriptError when the script fails.
     */
    handle(event: HookEvent | string, opts?: CallOptions): Promise<HandleResult> {
      return run("handle", resolvePath(manifestPath), event, opts);
    },
  };
}
