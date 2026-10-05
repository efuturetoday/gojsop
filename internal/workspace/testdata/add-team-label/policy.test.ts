import { describe, expect, test } from "vitest";
import { cluster, deployment, namespace, policy } from "@gojsop/testing";

const addTeamLabel = policy("./policy.yaml");

describe("add-team-label", () => {
  test("copies the team label from the namespace", async () => {
    const result = await addTeamLabel.review(
      { object: deployment({ name: "api", namespace: "shop" }) },
      { cluster: cluster([namespace("shop", { team: "checkout" })]) },
    );
    expect(result.allowed).toBe(true);
    expect(result.patchedObject?.metadata?.labels).toEqual({ team: "checkout" });
    expect(result.console.map((l) => l.text)).toEqual(["team is checkout"]);
  });

  test("labels an unowned namespace as unowned", async () => {
    const result = await addTeamLabel.review(
      { object: deployment({ name: "api", namespace: "shop" }) },
      { cluster: [namespace("shop")] },
    );
    expect(result.patchedObject?.metadata?.labels).toEqual({ team: "unowned" });
  });
});
