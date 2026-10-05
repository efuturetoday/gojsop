import type { KubeObject } from "./types.js";

type Meta = { labels?: Record<string, string>; annotations?: Record<string, string> };
type Ref = string | { name?: string; namespace?: string };

// metadata of a fixture: name, optional namespace, and only the maps that are set.
function meta(name: string, namespace: string | undefined, o: Meta): KubeObject["metadata"] {
  return {
    name,
    ...(namespace ? { namespace } : {}),
    ...(o.labels ? { labels: o.labels } : {}),
    ...(o.annotations ? { annotations: o.annotations } : {}),
  };
}

// "ns/name", "name" (namespace "default") or { name, namespace }
function ref(r: Ref, fallbackName: string): { name: string; namespace: string } {
  if (typeof r === "string") {
    const [a, b] = r.split("/");
    return b === undefined ? { namespace: "default", name: a } : { namespace: a, name: b };
  }
  return { namespace: r.namespace ?? "default", name: r.name ?? fallbackName };
}

/** A Pod with one container. Defaults: name "pod", namespace "default", image "nginx:1.27". */
export function pod(o: { name?: string; namespace?: string; image?: string } & Meta = {}): KubeObject {
  return {
    apiVersion: "v1",
    kind: "Pod",
    metadata: meta(o.name ?? "pod", o.namespace ?? "default", o),
    spec: { containers: [{ name: "app", image: o.image ?? "nginx:1.27" }] },
  };
}

/** A ConfigMap: `"ns/name"`, `"name"` (namespace "default") or `{ name, namespace }`. */
export function configMap(r: Ref, o: { data?: Record<string, string> } & Meta = {}): KubeObject {
  const { name, namespace } = ref(r, "config");
  return { apiVersion: "v1", kind: "ConfigMap", metadata: meta(name, namespace, o), data: o.data ?? {} };
}

/** A Secret, named like `configMap`; `data` values are base64 as in the API. */
export function secret(r: Ref, o: { data?: Record<string, string>; type?: string } & Meta = {}): KubeObject {
  const { name, namespace } = ref(r, "secret");
  return {
    apiVersion: "v1",
    kind: "Secret",
    metadata: meta(name, namespace, o),
    type: o.type ?? "Opaque",
    data: o.data ?? {},
  };
}

/** A Service with one port. Defaults: name "service", namespace "default", port 80. */
export function service(
  o: { name?: string; namespace?: string; port?: number; selector?: Record<string, string> } & Meta = {},
): KubeObject {
  return {
    apiVersion: "v1",
    kind: "Service",
    metadata: meta(o.name ?? "service", o.namespace ?? "default", o),
    spec: { selector: o.selector ?? {}, ports: [{ port: o.port ?? 80 }] },
  };
}

/** A Namespace (cluster-scoped) with optional labels. */
export function namespace(name: string, labels?: Record<string, string>): KubeObject {
  return { apiVersion: "v1", kind: "Namespace", metadata: meta(name, undefined, { labels }) };
}

/** A Deployment with one container. Defaults: name "deployment", namespace "default", 1 replica. */
export function deployment(
  o: { name?: string; namespace?: string; image?: string; replicas?: number } & Meta = {},
): KubeObject {
  const name = o.name ?? "deployment";
  const labels = { app: name };
  return {
    apiVersion: "apps/v1",
    kind: "Deployment",
    metadata: meta(name, o.namespace ?? "default", o),
    spec: {
      replicas: o.replicas ?? 1,
      selector: { matchLabels: labels },
      template: {
        metadata: { labels },
        spec: { containers: [{ name: "app", image: o.image ?? "nginx:1.27" }] },
      },
    },
  };
}
