# gojsop – Kubernetes hooks and admission policies in JavaScript

Automate and guard your Kubernetes cluster with a few lines of JavaScript.

- **Policies** check every request before the cluster stores it: refuse images
  tagged `:latest`, require an owner label, add defaults.
- **Hooks** react when something changes: copy a ConfigMap into new
  namespaces, label new Pods, clean up after a deleted object.

You write a function in TypeScript or JavaScript, test it with vitest, and
deploy it with `kubectl`. Each
script runs sandboxed inside gojsop, with a memory limit, a timeout and only
the rights you give it.

> **Alpha.** The API may still change. See [Known limitations](#known-limitations).

## Get started

You need Node.js 22 or newer. You need a cluster only for the last step.

### 1. Create a project

```sh
npm create @gojsop my-policies
cd my-policies
npm install
npm test
```

You get one example policy and one example hook, each in its own folder with
its tests:

```
my-policies/
  policies/no-latest/    policy.yaml, policy.ts, policy.test.ts
  hooks/count-pods/      hook.yaml, hook.ts, hook.test.ts
```

Add, rename or remove a policy or hook with one command each:

```sh
npx gojsop new policy require-owner     # or: new hook <name>; comes with a passing test
npx gojsop rn require-owner need-owner
npx gojsop rm need-owner
```

### 2. Write a policy

`policy.yaml` says which requests the policy sees, here every Pod that is
created or updated (shortened):

```yaml
apiVersion: core.gojsop.io/v1alpha1
kind: JSAdmission
metadata:
  name: no-latest
spec:
  rules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["pods"]
      operations: ["CREATE", "UPDATE"]
```

`policy.ts` decides:

```ts
import type { Request, Response } from "@gojsop/types";

export function validate(req: Request): Response {
  for (const c of req.object?.spec?.containers ?? []) {
    if (c.image?.endsWith(":latest")) {
      return { allowed: false, message: `${c.image} uses :latest` };
    }
  }
  return { allowed: true };
}
```

Tip: Kubernetes leaves out empty fields, so use `?.` and `??`. You can
import your own modules and npm packages; gojsop bundles them.

### 3. Test it

```ts
import { expect, test } from "vitest";
import { policy, pod } from "@gojsop/testing";

const noLatest = policy("./policy.yaml");

test("denies :latest", async () => {
  const result = await noLatest.review({ object: pod({ image: "nginx:latest" }) });
  expect(result.allowed).toBe(false);
});
```

Run `npx vitest`; it reruns the tests whenever you save. Your script runs
exactly as it would in the cluster, so a green test means it works there too.

### 4. Write a hook

A hook gets an event for every change of what it watches. This one counts
the Pods labeled `track: "true"` and writes the number into a ConfigMap:

```ts
import type { Event } from "@gojsop/types";

export function handle(event: Event) {
  const count = event.all().length;
  kube.apply({
    apiVersion: "v1",
    kind: "ConfigMap",
    metadata: { name: "pod-count", namespace: event.object.metadata?.namespace },
    data: { count: String(count) },
  });
}
```

Its test starts with a few Pods and checks the ConfigMap afterwards:

```ts
import { expect, test } from "vitest";
import { cluster, hook, pod } from "@gojsop/testing";

test("counts the tracked pods", async () => {
  const tracked = { track: "true" };
  const c = cluster([pod({ name: "a", labels: tracked }), pod({ name: "b", labels: tracked })]);

  await hook("./hook.yaml").handle({ object: pod({ name: "a", labels: tracked }) }, { cluster: c });

  expect(c.get("v1", "ConfigMap", "default", "pod-count")?.data).toEqual({ count: "2" });
});
```

A hook may only do what `hook.yaml` allows under `permissions`.

### 5. Deploy

Install gojsop once per cluster. It needs
[cert-manager](https://cert-manager.io):

```sh
helm repo add jetstack https://charts.jetstack.io
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace --set crds.enabled=true --wait
```

<!-- x-release-please-start-version -->
```sh
helm install gojsop oci://ghcr.io/efuturetoday/charts/gojsop \
  --namespace gojsop-system --create-namespace --wait \
  --version 0.1.1
```
<!-- x-release-please-end -->

Then deploy your project to the cluster `kubectl` points at:

```sh
npm run deploy                  # tests, builds dist/, kubectl apply -f dist/
kubectl get jsadmissions,jshooks
```

`npm run build` writes one file per hook and policy to `dist/`, with the
script inside. Change a script and deploy again; gojsop picks it up without
a restart. A policy you remove with `gojsop rm` stays in the cluster until
you run `kubectl delete jsadmission <name>`.

> **The example policy blocks nothing yet.** It starts with
> `enforcement: Audit` and only records what it would deny
> (`kubectl describe jsadmission no-latest`). Set `enforcement: Deny` when
> you are happy with it. Your tests always show what the script decides.

That's it. Every field and option is in the [reference](docs/reference.md).

## Writing scripts

Good to know once you write your own:

- **Export the entry point:** `export function validate`, `mutate` or
  `handle`. It must not be `async`.
- **Plain JavaScript works too:** name the file `policy.js` and write
  `function validate(req) {}` without `export`; it runs as it is.
- **No memory between calls.** Each call starts fresh. Keep state in the
  cluster.
- **Talking to the cluster:** `kube.get`, `kube.list`, `kube.apply` and
  `kube.delete`, each with one object, for example
  `kube.get({ apiVersion: "v1", kind: "ConfigMap", name: "x", namespace: "y" })`.
  Policies can only read.
- **Changing requests:** a policy with `type: mutating` defines `mutate(req)`,
  changes `req.object` and returns `{ allowed: true, modifiedObject: req.object }`.
- **Logging:** `console.log` shows in the test result and in the operator log.
- **Debugging one request:**
  `npx gojsop run policies/no-latest/policy.yaml --request policies/no-latest/pod.yaml --trace`
  prints the result and every `kube.*` call.
- **Limits:** 32 MB and 30 seconds per call by default.

The [reference](docs/reference.md#scripts) has the details.

## How gojsop compares

As of October 2026; each claim links to its source below the table.

| | gojsop | jsPolicy | Kyverno |
|---|---|---|---|
| Policies are written in | JavaScript | JavaScript or TypeScript, with npm packages [1] | YAML with CEL; JMESPath in the older, deprecated policy types [7] |
| Engine | QuickJS as WebAssembly, pure Go | V8 through cgo [2] | Go |
| Validate and mutate requests | yes | yes [1] | yes [7] |
| React to changes | with code (`JSHook`) | with code (controller policies) [1] | declaratively: generate and mutate existing resources [7] |
| Try before enforcing | `Audit` and `Warn` | `warn` [3] | `Audit`, with warnings on request [8] |
| Rights | each script has its own ServiceAccount with the rights it declares; nobody grants more than they hold | the operator runs as `cluster-admin` by default [4] | one ServiceAccount per Kyverno controller, extended by aggregated roles [9] |
| Namespaced policies | no | no [5] | yes [7] |
| Image signature checks | no | not in the docs [1] | yes [7] |
| Reports of existing resources | no, violations are events | denied requests are logged in `JsPolicyViolations` [5] | policy reports and background scans [10] |
| Local tests | vitest; the script runs in the operator's engine | Jest in Node, from the jspolicy-sdk template [6] | `kyverno test` with YAML test files [11] |
| Status | alpha | last release v0.3.0-beta.6, June 2024 [12] | stable, CNCF graduated [13] |

gojsop is for teams that want the freedom of real code, with each script
limited to exactly what it may touch. When a declarative policy language and
a large ecosystem matter more, use Kyverno.

<details>
<summary>Sources</summary>

1. jsPolicy README: https://github.com/loft-sh/jspolicy#readme
2. jsPolicy `go.mod` (rogchap.com/v8go): https://github.com/loft-sh/jspolicy/blob/main/go.mod
3. jsPolicy configuration, `violationPolicy: deny | warn`: https://github.com/loft-sh/jspolicy/blob/main/docs/pages/writing-policies/configuration.mdx
4. jsPolicy chart, `clusterRole: cluster-admin`: https://github.com/loft-sh/jspolicy/blob/main/chart/values.yaml
5. jsPolicy CRDs (`JsPolicy` is cluster-scoped; `JsPolicyViolations`): https://github.com/loft-sh/jspolicy/blob/main/chart/crds/crds.yaml
6. jsPolicy, testing policies: https://github.com/loft-sh/jspolicy/blob/main/docs/pages/writing-policies/testing-policies.mdx
7. Kyverno policy types: https://kyverno.io/docs/policy-types/overview/
8. Kyverno validate rules, `failureAction` and `emitWarning`: https://kyverno.io/docs/policy-types/cluster-policy/validate/
9. Kyverno installation, RBAC customization: https://kyverno.io/docs/installation/customization/
10. Kyverno policy reports: https://kyverno.io/docs/policy-reports/
11. Kyverno CLI, `kyverno test`: https://kyverno.io/docs/kyverno-cli/reference/kyverno_test/
12. jsPolicy releases: https://github.com/loft-sh/jspolicy/releases
13. CNCF, Kyverno: https://www.cncf.io/projects/kyverno/

</details>

## Known limitations

- **Alpha.** Fields may still change, without a migration path.
- **The operator holds a lot of power.** It hands each script its rights, so
  it may create roles. Protect its namespace like `kube-system`.
- **Hooks can miss a deletion** that happens while the operator switches its
  leader replica. Changes are never missed.
- **A hook that always fails is retried forever.**

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache 2.0](LICENSE)
