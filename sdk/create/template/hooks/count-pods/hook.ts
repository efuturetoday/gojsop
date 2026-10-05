import type { Event } from "@gojsop/types";

// Writes the number of tracked pods into a ConfigMap of the pod's namespace.
export function handle(event: Event) {
  const count = event.all().length;
  kube.apply({
    apiVersion: "v1",
    kind: "ConfigMap",
    metadata: { name: "pod-count", namespace: event.object.metadata?.namespace },
    data: { count: String(count) },
  });
  return { counted: count };
}
