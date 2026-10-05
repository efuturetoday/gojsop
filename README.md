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

```bash
npm create @gojsop my-policies
cd my-policies
npm install
npm test
```

The project starts with two examples you can read and delete:
`policies/no-latest` and `hooks/count-pods`.

### 2. Add a policy

```bash
npx gojsop new policy require-owner
```

This creates `policies/require-owner/` with three files: the manifest, the
script and a test that already passes.

`policy.yaml` says which requests the policy sees. The new one sees every Pod
that is created or updated:

```yaml
spec:
  rules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["pods"]
      operations: ["CREATE", "UPDATE"]
```

`policy.ts` decides. Make it require an owner label:

```ts
import type { Request, Response } from "@gojsop/types";

export function validate(req: Request): Response {
  if (req.object?.metadata?.labels?.owner) {
    return { allowed: true };
  }
  return { allowed: false, message: "every pod needs an owner label" };
}
```

Tip: Kubernetes leaves out empty fields, so use `?.` and `??`. You can
import your own modules and npm packages; gojsop bundles them.

### 3. Test it

Write the cases into `policy.test.ts`:

```ts
import { expect, test } from "vitest";
import { pod, policy } from "@gojsop/testing";

const requireOwner = policy("./policy.yaml");

test("denies a pod without owner", async () => {
  const result = await requireOwner.review({ object: pod() });
  expect(result.allowed).toBe(false);
});

test("allows a pod with owner", async () => {
  const result = await requireOwner.review({ object: pod({ labels: { owner: "team-a" } }) });
  expect(result.allowed).toBe(true);
});
```

Run `npx vitest`; it reruns the tests whenever you save. Your script runs
exactly as it would in the cluster, so a green test means it works there too.

A test can also take a YAML file, for example a Pod you copied from the
cluster with `kubectl get pod web -o yaml > pod.yaml`:

```ts
const result = await requireOwner.review("./pod.yaml");
```

### 4. Add a hook

A hook reacts to changes. This one copies the ConfigMap `default/shared` into
every new namespace:

```bash
npx gojsop new hook copy-config
```

In `hook.yaml`, say what the hook watches and what it may do:

```yaml
spec:
  bindings:
    - name: namespaces
      apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["namespaces"]
      events: ["Added"]
  permissions:
    - apiGroups: [""]
      resources: ["configmaps"]
      verbs: ["get", "create", "patch"]
```

`hook.ts`:

```ts
import type { Event } from "@gojsop/types";

export function handle(event: Event) {
  const shared = kube.get({ apiVersion: "v1", kind: "ConfigMap", namespace: "default", name: "shared" });
  if (!shared) return;
  kube.apply({
    apiVersion: "v1",
    kind: "ConfigMap",
    metadata: { name: "shared", namespace: event.object.metadata?.name },
    data: shared.data,
  });
}
```

Its test starts with a cluster in memory and checks it afterwards:

```ts
import { expect, test } from "vitest";
import { cluster, configMap, hook, namespace } from "@gojsop/testing";

test("copies the shared ConfigMap into a new namespace", async () => {
  const c = cluster([configMap("default/shared", { data: { color: "blue" } })]);

  await hook("./hook.yaml").handle({ object: namespace("team-a") }, { cluster: c });

  expect(c.get("v1", "ConfigMap", "team-a", "shared")?.data).toEqual({ color: "blue" });
});
```

A hook may only do what `permissions` allows; anything else fails, in the
test as in the cluster.

### 5. Deploy

Install gojsop once per cluster. It needs
[cert-manager](https://cert-manager.io):

```bash
helm repo add jetstack https://charts.jetstack.io
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace --set crds.enabled=true --wait
```

<!-- x-release-please-start-version -->
```bash
helm install gojsop oci://ghcr.io/efuturetoday/charts/gojsop \
  --namespace gojsop-system --create-namespace --wait \
  --version 0.2.0
```
<!-- x-release-please-end -->

Then deploy your project to the cluster `kubectl` points at:

```bash
npm run deploy                  # tests, builds dist/, kubectl apply -f dist/
kubectl get jsadmissions,jshooks
```

`npm run build` writes one file per hook and policy to `dist/`, with the
script inside. Change a script and deploy again; gojsop picks it up without
a restart.

Rename or remove a policy or hook with one command:

```bash
npx gojsop rn require-owner need-owner
npx gojsop rm need-owner
```

What you remove stays in the cluster until you delete it there, for example
`kubectl delete jsadmission need-owner`.

> **A new policy blocks nothing yet.** `gojsop new` sets
> `enforcement: Audit`: the policy only records what it would deny
> (`kubectl describe jsadmission require-owner`). Set `enforcement: Deny` in
> `policy.yaml` when you are happy with it. Your tests always show what the
> script decides.

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
