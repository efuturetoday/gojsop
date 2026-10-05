# @gojsop/testing

Test [gojsop](https://github.com/efuturetoday/gojsop) hooks and policies with
[vitest](https://vitest.dev). Every `review` and `handle` call goes to the
`gojsop` CLI (`gojsop serve --stdio`), which runs your script in the engine the
operator uses, against a cluster held in memory. The script never runs in
Node.

## Install

```sh
npm install --save-dev vitest @gojsop/testing
```

The package needs the `gojsop` binary: put it on `PATH` or set `GOJSOP_BIN` to
its path. One `gojsop serve` process starts per vitest worker, on the first
call, and ends with the worker.

## Test a policy

```ts
import { expect, test } from "vitest";
import { policy, pod } from "@gojsop/testing";

const noLatest = policy("./policy.yaml"); // relative to this test file

test("denies :latest", async () => {
  const result = await noLatest.review({ object: pod({ image: "nginx:latest" }) });
  expect(result.allowed).toBe(false);
  expect(result.message).toMatch(/uses :latest/);
  // result.warnings, result.patchedObject (mutating policies), result.console
});
```

## Test a hook

```ts
import { expect, test } from "vitest";
import { cluster, hook, pod } from "@gojsop/testing";

test("counts the tracked pods", async () => {
  const tracked = { track: "true" };
  const c = cluster([pod({ name: "a", labels: tracked }), pod({ name: "b", labels: tracked })]);

  const result = await hook("./hook.yaml").handle({ object: pod({ name: "a", labels: tracked }) }, { cluster: c });

  expect(result.return).toEqual({ counted: 2 });
  // After the call the cluster holds what the hook wrote.
  expect(c.get("v1", "ConfigMap", "default", "pod-count")?.data).toEqual({ count: "2" });
});
```

## API

- `policy(path).review(request, { cluster? })` resolves to `ReviewResult`
  (`allowed`, `message`, `warnings`, `modifiedObject`, `patchedObject`, `console`).
- `hook(path).handle(event, { cluster? })` resolves to `HandleResult`
  (`return`, `console`).
- `path` is a manifest or its directory; a relative path is relative to the
  test file.
- `cluster(objects?)` is the fake cluster: `add(obj)`, `get(apiVersion, kind,
  namespace, name)`, `list(apiVersion, kind, namespace?)`, `objects()`. Pass it
  as `cluster` (or pass a plain array); a `cluster()` is updated after the call.
- Fixtures with small defaults: `pod`, `configMap`, `secret`, `service`,
  `namespace`, `deployment`.
- A script that throws, runs out of time or memory, or a bad input, makes the
  call reject with `ScriptError`; its `kind` is `script`, `timeout`,
  `memoryLimit` or `input`:
  `await expect(p.review(req)).rejects.toThrow(/timeout/)`. A `kube.*` call
  without the right in `spec.permissions` is a `script` error with `Forbidden`.
