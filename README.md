# gojsop

gojsop is a Kubernetes operator that runs your JavaScript inside the cluster.

- A **`JSHook`** watches resources and calls your `handle()` for every change,
  like [shell-operator](https://github.com/flant/shell-operator) hooks, but in
  JavaScript and declared as a custom resource.
- A **`JSAdmission`** turns your `validate()` or `mutate()` into an admission
  webhook: the apiserver asks your script before it stores an object.

The scripts run in [QuickJS](https://github.com/quickjs-ng/quickjs) compiled to
WebAssembly and embedded in the operator. There is no Node.js, no sidecar and
no cgo; each script gets its own sandbox with a memory limit and a timeout.

> **Status: alpha.** The API is `core.gojsop.io/v1alpha1` and may still change
> in incompatible ways. See [Known limitations](#known-limitations).

## Quick start

You need a Kubernetes cluster, `kubectl`, Docker and Go 1.25+.

```sh
# 1. cert-manager issues the webhook certificate
make install-certmanager

# 2. build the image and push it where the cluster can pull it
make docker-build docker-push IMG=<registry>/gojsop:dev

# 3. install CRDs, RBAC and the operator into gojsop-system
make deploy IMG=<registry>/gojsop:dev

# 4. try the samples: a ConfigMap sync hook and two pod policies
kubectl apply -k config/samples/
kubectl get jshooks,jsadmissions
```

On [kind](https://kind.sigs.k8s.io/), skip the push and load the image instead:
`make docker-build IMG=gojsop:dev && kind load docker-image gojsop:dev`.

Remove everything with `kubectl delete -k config/samples/` and `make undeploy`.

## JSHook

A hook names the resources it watches in `spec.bindings` and defines
`handle()` in `spec.source`:

```yaml
apiVersion: core.gojsop.io/v1alpha1
kind: JSHook
metadata:
  name: label-logger
spec:
  bindings:
    - name: pods
      apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["pods"]
      events: ["Added", "Deleted"]          # default: all three
      namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: default
  source:
    inline: |
      function handle(contexts) {
        for (const ctx of contexts) {
          if (ctx.type === "Synchronization") {
            console.log(ctx.binding, "starts with", ctx.objects.length, "pods");
          } else {
            console.log(ctx.watchEvent, ctx.object.metadata.name);
          }
        }
      }
```

**What `handle()` receives.** Each call gets an array with one context:

| `type` | When | Fields |
|---|---|---|
| `Synchronization` | once per binding, when its watch starts | `binding`, `objects: [{object}, ...]` — every matching object that exists |
| `Event` | once per change after that | `binding`, `watchEvent` (`Added`, `Modified`, `Deleted`), `object` |

"Synchronization" is the start state: your hook sees every existing object
once, then only the changes. Changes during the synchronization are not lost.
With `synchronization: false` there is no synchronization; existing objects
arrive as `Added` events instead.

**Guarantees.**

- Calls of one hook never run at the same time.
- Several waiting changes of one object fold into one call with the newest
  state.
- A call that throws, times out or hits the memory limit records a Warning
  event on the `JSHook` and is retried with backoff. A retry never overwrites
  a newer state of the same object.
- A binding names a concrete group, version and resource. `*`, unknown
  resources and two bindings on one resource are rejected; the hook keeps the
  watches it had.

## JSAdmission

A policy names the requests it guards in `spec.rules` and defines
`validate(req)` (`type: validating`, the default) or `mutate(req)`
(`type: mutating`):

```yaml
apiVersion: core.gojsop.io/v1alpha1
kind: JSAdmission
metadata:
  name: prevent-latest-tags
spec:
  rules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["pods"]
      operations: ["CREATE", "UPDATE"]
  failurePolicy: Fail        # or Ignore
  timeoutSeconds: 5          # 1 to 30
  source:
    inline: |
      function validate(req) {
        for (const c of req.object.spec.containers || []) {
          if (c.image.endsWith(":latest")) {
            return { allowed: false, message: c.image + " uses :latest" };
          }
        }
        return { allowed: true };
      }
```

**The request.** `req` has `uid`, `kind`, `resource`, `subResource`, `name`,
`namespace`, `operation`, `userInfo`, `object`, `oldObject` and `dryRun`.

**The answer.** Return `{ allowed, message?, code?, warnings? }`. A mutating
policy changes `req.object` and returns it as `modifiedObject`; gojsop sends
the difference as a JSON patch. Server-managed fields and `/status` are never
patched. A result without `allowed` denies.

**When the script fails** — it throws, times out, hits the memory limit, or
no script is ready yet — `failurePolicy` decides: `Ignore` admits the request
with a warning, `Fail` denies it with code 500. The call ends at the smaller of
`spec.timeoutSeconds` and `spec.limits.timeoutSeconds`.

Every operator replica answers every policy, so scaling the operator never
leaves a replica that answers with 404.

No policy ever sees requests in the operator's namespace, `kube-system` or
`cert-manager`, so a broken policy cannot lock up the cluster itself. Change
the list with the operator flag `--admission-exclude-namespaces`.

## Writing scripts

**Entry points are global functions.** Write `function handle() {}` (or
`var handle = ...`). A top-level `const`, `let` or `class`, and code a bundler
wrapped in a closure, are not visible to gojsop; the script then fails with
`missing required export: handle()`.

**No state between calls.** gojsop runs the top-level code once and takes a
snapshot. Every call starts from that snapshot, so globals you change inside
`handle()` are gone at the next call. Keep state in the cluster.

**Reaching the cluster.** The global `kube` takes one object per call:

| Function | Hooks | Policies | Does |
|---|---|---|---|
| `kube.get({apiVersion, kind, name, namespace})` | yes | yes | the object, or `null` when it is missing |
| `kube.list({apiVersion, kind, namespace, labelSelector, fieldSelector})` | yes | yes | the items; an empty `namespace` lists all namespaces |
| `kube.apply(object)` | yes | — | creates the object, or sends a JSON merge patch (not server-side apply) |
| `kube.delete({apiVersion, kind, name, namespace})` | yes | — | deletes; a missing object is no error |

A failed call throws, and the script may catch it. Policies can only read:
admission must not change the cluster.

**Logging.** `console.log`, `info`, `debug`, `warn` and `error` go to the
operator log; `warn` and `error` also become Warning events on the hook or
policy (`kubectl describe jshook <name>`).

**Limits.** `spec.limits.memoryMB` (default 32, at most 512) and
`spec.limits.timeoutSeconds` (default 30, at most 300) apply to every call
and to loading the script. A script stuck in a loop is stopped at the
deadline; `try/catch` cannot hold it.

**Sources.** Inline in `spec.source.inline`, or from a ConfigMap:

```yaml
source:
  configMapRef:
    name: my-hook
    namespace: default   # required: JSHook and JSAdmission are cluster-scoped
    key: hook.js         # default
```

A ConfigMap edit reaches the script without a restart. `spec.source.oci` is
in the schema but not implemented yet.

## Status

`kubectl get jshooks` and `kubectl get jsadmissions` show a `Ready` condition:

| Reason | Means |
|---|---|
| `Reconciled` | the script is ready and serving |
| `Building` | a new script is being prepared (new source, new limits or a manual restart) |
| `BuildFailed` | the script did not load; retried with backoff, a new source is tried at once |
| `WebhookSyncFailed` | gojsop could not write the webhook configuration; retried |
| `Failed` | anything else, see the message and the events |

Set the annotation `gojsop.io/restart` to a new value to prepare the script
again.

## Known limitations

- **Broad permissions.** Every script acts as the operator's ServiceAccount,
  which may read and write every resource. Only install hooks you trust.
- **Failover gap.** Hooks run on the leader only. After a leader change the
  new leader starts with a fresh synchronization: objects that changed in
  between arrive in it, but a deletion in between is never seen.
- **Endless retries.** A hook that always fails is retried forever.
- **`v1alpha1`.** Fields may change without a migration path.

## Development

```sh
make test          # unit and envtest integration tests
make lint          # golangci-lint
make test-e2e      # e2e tests on a throwaway kind cluster
make engine-wasm   # rebuild the embedded QuickJS engine (after glue.c changes)
make help          # every target
```

How the project is organised, its rules and its open items live in
[`.agents/`](.agents/README.md); [AGENTS.md](AGENTS.md) is the entry point.

`make sdlc-check` checks `.agents/` against the code. It runs a tool from the
private repository `github.com/efuturetoday/agentic-sdlc`; the target sets
`GOPRIVATE`, and Git needs credentials for GitHub, for example:

```sh
git config --global credential.https://github.com.helper '!gh auth git-credential'
```

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
