/** A Kubernetes object; only what gojsop needs is typed, the rest is open. */
export interface KubeObject {
  apiVersion: string;
  kind: string;
  metadata?: {
    name?: string;
    namespace?: string;
    labels?: Record<string, string>;
    annotations?: Record<string, string>;
    [key: string]: unknown;
  };
  [key: string]: unknown;
}

/** The request a policy's `validate(req)` or `mutate(req)` gets. */
export interface AdmissionRequest {
  /** CREATE (default), UPDATE, DELETE or CONNECT. */
  operation?: string;
  object?: KubeObject;
  oldObject?: KubeObject;
  userInfo?: { username?: string; uid?: string; groups?: string[]; extra?: Record<string, string[]> };
  /** Defaults to the namespace of the object. */
  namespace?: string;
  /** Defaults to the name of the object. */
  name?: string;
  dryRun?: boolean;
}

/** One line the script wrote with `console.*`. */
export interface ConsoleLine {
  level: string;
  text: string;
}

/** What a policy decided. */
export interface ReviewResult {
  allowed: boolean;
  code?: number;
  message?: string;
  warnings?: string[];
  /** What a mutating policy returned as `modifiedObject`. */
  modifiedObject?: KubeObject;
  /** The object after the patch the operator would send; a mutating policy only. */
  patchedObject?: KubeObject;
  console: ConsoleLine[];
}

/** The event a hook's `handle(event)` gets. */
export interface HookEvent {
  /** Added (default), Modified or Deleted. */
  type?: string;
  object: KubeObject;
  /** Defaults to the first binding that watches the object's kind. */
  binding?: string;
  initial?: boolean;
}

/** What a hook returned. */
export interface HandleResult {
  /** The value `handle` returned, as JSON; undefined when it returned nothing. */
  return?: unknown;
  console: ConsoleLine[];
}

export type ErrorKind = "script" | "timeout" | "memoryLimit" | "input";
