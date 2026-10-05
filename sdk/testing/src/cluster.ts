import type { KubeObject } from "./types.js";

const ns = (o: KubeObject) => o.metadata?.namespace ?? "";

/**
 * The fake cluster of a test: objects held in memory. `kube.*` and
 * `event.all()` of the script read and write it. After `handle` or `review`
 * it holds what the script left.
 */
export class Cluster {
  private items: KubeObject[] = [];

  constructor(objects: KubeObject[] = []) {
    objects.forEach((o) => this.add(o));
  }

  /** Adds an object, replacing the one with the same kind, namespace and name. */
  add(obj: KubeObject): this {
    const i = this.items.findIndex((o) => this.same(o, obj.apiVersion, obj.kind, ns(obj), obj.metadata?.name));
    if (i >= 0) this.items[i] = obj;
    else this.items.push(obj);
    return this;
  }

  /** The object, or undefined. Leave `namespace` empty for cluster-scoped kinds. */
  get(apiVersion: string, kind: string, namespace: string, name: string): KubeObject | undefined {
    return this.items.find((o) => this.same(o, apiVersion, kind, namespace, name));
  }

  /** Every object of the kind, in one namespace or in all. */
  list(apiVersion: string, kind: string, namespace?: string): KubeObject[] {
    return this.items.filter(
      (o) => o.apiVersion === apiVersion && o.kind === kind && (namespace === undefined || ns(o) === namespace),
    );
  }

  /** All objects. */
  objects(): KubeObject[] {
    return [...this.items];
  }

  /** @internal Replaces the content with what a call left behind. */
  replace(objects: KubeObject[]): void {
    this.items = objects;
  }

  private same(o: KubeObject, apiVersion: string, kind: string, namespace: string, name?: string): boolean {
    return o.apiVersion === apiVersion && o.kind === kind && ns(o) === namespace && o.metadata?.name === name;
  }
}

/** Creates a fake cluster, optionally with objects. */
export function cluster(objects: KubeObject[] = []): Cluster {
  return new Cluster(objects);
}
