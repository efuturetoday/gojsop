import { expect, test } from "vitest";
import { cluster, hook, pod } from "@gojsop/testing";

const countPods = hook("./hook.yaml");

test("counts only the pods its binding selects", async () => {
  const tracked = { track: "true" };
  const c = cluster([
    pod({ name: "a", labels: tracked }),
    pod({ name: "b", labels: tracked }),
    pod({ name: "c" }),
  ]);

  const result = await countPods.handle({ object: pod({ name: "a", labels: tracked }) }, { cluster: c });

  expect(result.return).toEqual({ counted: 2 });
  // The hook wrote a ConfigMap; the fake cluster shows it.
  expect(c.get("v1", "ConfigMap", "default", "pod-count")?.data).toEqual({ count: "2" });
});
