// Types for gojsop scripts. A policy exports validate(req) or mutate(req), a
// hook exports handle(event); the global kube reaches the cluster with the
// rights in spec.permissions.

/** A Kubernetes object; only what every object has is typed. */
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
  // Loose on purpose: a script handles any kind of object.
  // biome-ignore lint/suspicious/noExplicitAny: spec differs per kind; any keeps scripts short
  spec?: any;
  // biome-ignore lint/suspicious/noExplicitAny: status differs per kind
  status?: any;
  data?: Record<string, string>;
  [key: string]: unknown;
}

/** What a policy's validate(req) or mutate(req) gets. */
export interface Request {
  uid: string;
  kind: { group: string; version: string; kind: string };
  resource: { group: string; version: string; resource: string };
  subResource?: string;
  name?: string;
  namespace?: string;
  operation: "CREATE" | "UPDATE" | "DELETE" | "CONNECT";
  userInfo: { username?: string; uid?: string; groups?: string[]; extra?: Record<string, string[]> };
  /** Missing on DELETE. */
  object?: KubeObject;
  /** Missing on CREATE. */
  oldObject?: KubeObject;
  dryRun: boolean;
}

/** What validate or mutate returns. A result without allowed denies. */
export interface Response {
  allowed: boolean;
  message?: string;
  /** HTTP status code of a denial, e.g. 403. */
  code?: number;
  warnings?: string[];
  /** mutate only: the changed object; gojsop sends the difference as a JSON patch. */
  modifiedObject?: KubeObject;
}

/** What a hook's handle(event) gets. */
export interface Event {
  type: "Added" | "Modified" | "Deleted";
  /** For Deleted, its last state. */
  object: KubeObject;
  /** The name of the binding that saw the change. */
  binding: string;
  /** true for an object that existed when the hook started watching. */
  initial: boolean;
  /** Every object the binding watches right now. */
  all(): KubeObject[];
}

export interface ObjectRef {
  apiVersion: string;
  kind: string;
  name: string;
  namespace?: string;
}

export interface ListOptions {
  apiVersion: string;
  kind: string;
  /** Empty or missing: all namespaces. */
  namespace?: string;
  labelSelector?: string;
  fieldSelector?: string;
}

/** The cluster, with the rights in spec.permissions. A failed call throws. */
export interface Kube {
  /** The object, or null when it is missing. */
  get(ref: ObjectRef): KubeObject | null;
  list(options: ListOptions): KubeObject[];
  /** Hooks only: creates the object or merges it into the existing one; returns what was stored. */
  apply(object: KubeObject): KubeObject;
  /** Hooks only: a missing object is no error. */
  delete(ref: ObjectRef): boolean;
}

declare global {
  const kube: Kube;
  // gojsop's console; merges with the DOM's or Node's when those are loaded.
  interface Console {
    log(...data: unknown[]): void;
    info(...data: unknown[]): void;
    debug(...data: unknown[]): void;
    warn(...data: unknown[]): void;
    error(...data: unknown[]): void;
  }
  var console: Console;
}
