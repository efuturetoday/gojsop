import { pod, policy } from "@gojsop/testing";
import { describe, expect, test } from "vitest";

const noLatest = policy("./policy.yaml");

describe("no-latest", () => {
  test("denies :latest", async () => {
    const result = await noLatest.review({ object: pod({ image: "nginx:latest" }) });
    expect(result.allowed).toBe(false);
    expect(result.message).toMatch(/uses :latest/);
  });

  test("allows a pinned image", async () => {
    const result = await noLatest.review({ object: pod({ image: "nginx:1.27" }) });
    expect(result.allowed).toBe(true);
  });

  test("warns about an untagged image", async () => {
    const result = await noLatest.review({ object: pod({ image: "nginx" }) });
    expect(result.allowed).toBe(true);
    expect(result.warnings).toEqual(["container app has no tag"]);
  });
});
