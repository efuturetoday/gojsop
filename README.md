# gojsop – Kubernetes hooks and admission policies in JavaScript

Automate and guard your cluster with a few lines of JavaScript, instead of
writing a Go controller or learning a policy language.

- **React to changes with a `JSHook`.** Copy a ConfigMap into every namespace
  that asks for it, label new Pods, clean up what a deleted object left
  behind. You write `handle(event)`; gojsop watches the resources you name and
  calls it for every change.
- **Allow, deny or fix requests with a `JSAdmission`.** Refuse images tagged
  `:latest`, require an owner label, add defaults. You write `validate(req)`
  or `mutate(req)`; the apiserver asks it before it stores an object.

Both are plain Kubernetes resources: you apply them with `kubectl`, Helm or
GitOps, and read their state with `kubectl get`. Each script runs sandboxed in
the operator ([QuickJS](https://github.com/quickjs-ng/quickjs) compiled to
WebAssembly, no Node.js, no sidecar), with a memory limit, a timeout and
exactly the cluster rights you grant it.

> **Status: alpha.** The API is `core.gojsop.io/v1alpha1` and may still change
> in incompatible ways. See [Known limitations](#known-limitations).

## Quick start

You need a Kubernetes cluster with [cert-manager](https://cert-manager.io),
which issues the webhook certificate:

```sh
helm repo add jetstack https://charts.jetstack.io
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace --set crds.enabled=true --wait
```

Install gojsop:

<!-- x-release-please-start-version -->
```sh
helm install gojsop oci://ghcr.io/efuturetoday/charts/gojsop \
  --namespace gojsop-system --create-namespace --wait \
  --version 0.1.1
```
<!-- x-release-please-end -->

Or without Helm:

<!-- x-release-please-start-version -->
```sh
kubectl apply -f https://github.com/efuturetoday/gojsop/releases/download/v0.1.1/install.yaml
```
<!-- x-release-please-end -->

Try the samples, a ConfigMap sync hook and two pod policies:

<!-- x-release-please-start-version -->
```sh
kubectl apply -k "github.com/efuturetoday/gojsop/config/samples?ref=v0.1.1"
kubectl get jshooks,jsadmissions
```
<!-- x-release-please-end -->

## JSHook

A hook names the resources it watches in `spec.bindings` and defines
`handle()` in `spec.source`. Every field, with the values it takes:

```yaml
apiVersion: core.gojsop.io/v1alpha1
kind: JSHook
metadata:
  name: label-logger                 # cluster-scoped: no namespace
  annotations:
    gojsop.io/restart: "2026-10-05"  # optional; any new value prepares the script again
spec:
  bindings:                          # required, 1 to 32
    - name: pods                     # required, unique in the hook, at most 63 characters
      apiGroups: [""]                # required, concrete: "" is the core group, no "*"
      apiVersions: ["v1"]            # required, concrete, no "*"
      resources: ["pods"]            # required, plural names, no "*"; one resource per binding in a hook
      events: ["Added", "Deleted"]   # optional: Added | Modified | Deleted; default all three
      namespaceSelector:             # optional LabelSelector on the object's namespace;
        matchLabels:                 #   one namespace: kubernetes.io/metadata.name
          kubernetes.io/metadata.name: default
      objectSelector:                # optional LabelSelector on the object itself
        matchExpressions:            #   matchLabels and matchExpressions
          - { key: app, operator: Exists }   # In | NotIn | Exists | DoesNotExist
  permissions:                       # optional, at most 64: what kube.* may do, see Permissions
    - apiGroups: [""]                # required, no "*"
      resources: ["configmaps"]      # required, no "*"; subresources like "deployments/scale"
      verbs: ["get", "list"]         # required: get | list | watch | create | update | patch | delete
  source:                            # required: exactly one of inline or configMapRef
    inline: |                        # at most 512 KiB
      function handle(event) {
        console.log(event.type, event.object.metadata.name,
                    event.initial ? "(was there before)" : "");
        console.log("pods in default now:", event.all().length);
      }
    # configMapRef:                  # instead of inline
    #   name: my-hook                # required
    #   namespace: default           # required
    #   key: hook.js                 # optional, default hook.js
  limits:                            # optional
    memoryMB: 32                     # 1 to 512, default 32: heap of one call
    timeoutSeconds: 30               # 1 to 300, default 30: one call, and loading the script
```

**What `handle()` receives.** One event per call:

| Field | Means |
|---|---|
| `event.type` | `Added`, `Modified` or `Deleted` |
| `event.object` | the object; for `Deleted` its last state |
| `event.binding` | the name of the binding that saw it |
| `event.initial` | `true` for an object that already existed when the hook started watching |
| `event.all()` | every object the binding watches right now, from the watch's cache, without a call to the apiserver |

Objects that exist when the hook starts arrive first, as `Added` with
`initial: true`; after that every change arrives, and nothing that happens in
between is lost. Most hooks only look at `event.object`. A hook that needs the
whole picture, for example to delete copies whose original is gone, calls
`event.all()`.

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
`validate(req)` or `mutate(req)`. Every field, with the values it takes:

```yaml
apiVersion: core.gojsop.io/v1alpha1
kind: JSAdmission
metadata:
  name: prevent-latest-tags          # cluster-scoped: no namespace
  annotations:
    gojsop.io/restart: "2026-10-05"  # optional; any new value prepares the script again
spec:
  type: validating                   # validating (calls validate) | mutating (calls mutate); default validating
  rules:                             # required, 1 to 32
    - apiGroups: [""]                # required; "*" allowed here
      apiVersions: ["v1"]            # required; "*" allowed
      resources: ["pods"]            # required; "*" and subresources ("pods/exec") allowed
      operations: ["CREATE", "UPDATE"]  # required: CREATE | UPDATE | DELETE | CONNECT | *
      scope: "*"                     # optional: * | Namespaced | Cluster; default *
  namespaceSelector: {}              # optional LabelSelector on the request's namespace
  objectSelector: {}                 # optional LabelSelector on the object
  matchPolicy: Equivalent            # Exact | Equivalent; default Equivalent
  enforcement: Deny                  # Deny | Warn | Audit; default Deny: what a denial does, see below
  failurePolicy: Fail                # Fail | Ignore; default Fail: what a failed call means (Warn and Audit: always Ignore)
  timeoutSeconds: 5                  # 1 to 30, default 5: how long the apiserver waits
  permissions:                       # optional: what kube.get and kube.list may read
    - apiGroups: [""]
      resources: ["namespaces"]
      verbs: ["get"]                 # get | list | watch only: a policy never writes
  source:                            # required: exactly one of inline or configMapRef
    inline: |
      function validate(req) {
        // Fields Kubernetes leaves empty are missing, and req.object is
        // null on DELETE: reach into objects with ?. and ??.
        for (const c of req.object?.spec?.containers ?? []) {
          if (c.image?.endsWith(":latest")) {
            return { allowed: false, message: c.image + " uses :latest" };
          }
        }
        return { allowed: true };
      }
  limits:                            # optional
    memoryMB: 32                     # 1 to 512, default 32
    timeoutSeconds: 30               # 1 to 300, default 30; a call ends at the smaller of this and spec.timeoutSeconds
```

**The request.** `req` has `uid`, `kind`, `resource`, `subResource`, `name`,
`namespace`, `operation`, `userInfo`, `object`, `oldObject` and `dryRun`.

**The answer.** Return `{ allowed, message?, code?, warnings? }`. A mutating
policy changes `req.object` and returns it as `modifiedObject`; gojsop sends
the difference as a JSON patch. Server-managed fields and `/status` are never
patched. A result without `allowed` denies.

**Try a policy before you enforce it.** `enforcement` decides what a denial
of the script does:

| `enforcement` | The request | The person who sent it sees | The policy shows |
|---|---|---|---|
| `Deny` (default) | is rejected | the reason, as the error | a `PolicyViolation` event |
| `Warn` | is admitted | the reason, as a warning | a `PolicyViolation` event |
| `Audit` | is admitted | nothing | a `PolicyViolation` event |

Start a new policy with `Audit`, read what it would deny with
`kubectl describe jsadmission <name>`, move to `Warn` to tell the teams, then
to `Deny`. Under `Warn` and `Audit` gojsop never denies, not even when the
script fails.

**When the script fails** — it throws, times out, hits the memory limit, or
no script is ready yet — `failurePolicy` decides: `Ignore` admits the request
with a warning, `Fail` denies it with code 500. The call ends at the smaller of
`spec.timeoutSeconds` and `spec.limits.timeoutSeconds`.

Every operator replica answers every policy, so scaling the operator never
leaves a replica that answers with 404.

No policy ever sees requests in the operator's namespace, `kube-system` or
`cert-manager`, so a broken policy cannot lock up the cluster itself. Change
the list with the operator flag `--admission-exclude-namespaces`.

## Test without a cluster

The `gojsop` CLI runs a hook or policy in the same engine as the operator,
against a cluster held in memory, with the script's own rights and limits.
Download it from the [release](https://github.com/efuturetoday/gojsop/releases),
put each hook or policy in a directory of its own, and add test cases:

```
policies/no-latest/
  policy.yaml            # the JSAdmission, without spec.source
  policy.js              # the script
  tests/denies-latest.yaml
```

```yaml
# tests/denies-latest.yaml
name: denies :latest
request:
  operation: CREATE
  object:
    apiVersion: v1
    kind: Pod
    metadata: { name: web, namespace: team-a }
    spec: { containers: [{ name: web, image: nginx:latest }] }
expect:
  allowed: false
  message: /uses :latest/    # exact text, or a regular expression between slashes
```

```sh
gojsop test                          # every case below the current directory
gojsop run policies/no-latest/policy.yaml --request pod.yaml --trace
```

A hook case gives an `event` and the `cluster` before, and expects objects
in the `cluster` after; `kube.*` without the right in `spec.permissions`
throws `Forbidden`, as in the cluster. `expect.error` expects the script to
fail, for example `/timeout/`.

## Permissions

Every hook and policy acts as a ServiceAccount of its own, which gojsop
creates in its namespace (`jshook-<name>`, `jsadmission-<name>`) and removes
with the resource. It gets exactly what `spec.permissions` lists, and a hook
also gets `get`, `list` and `watch` on what its bindings watch:

```yaml
permissions:
  - apiGroups: [""]               # "" is the core group
    resources: ["configmaps"]
    verbs: ["get", "create", "patch"]
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list"]
```

- **No wildcards.** Name every group and resource; `*` is rejected.
- **Policies only read.** A `JSAdmission` may list `get`, `list` and `watch`.
- **You cannot hand out what you do not have.** gojsop rejects a hook or
  policy whose rights the person applying it does not hold. Changing the
  script of an existing hook needs its rights too; changing only labels or
  annotations does not.
- **A missing right is visible.** A `kube.*` call without it throws
  `Forbidden`; the script can catch it. `status.serviceAccount` names the
  ServiceAccount, so `kubectl auth can-i --as=system:serviceaccount:gojsop-system:jshook-<name> ...`
  shows what it may do.

> **Protect a source ConfigMap like the hook itself.** Whoever may edit the
> ConfigMap a hook loads its script from changes what runs with the hook's
> rights, and gojsop does not check that edit.

## Writing scripts

**Entry points are global functions.** Write `function handle() {}` (or
`var handle = ...`). A top-level `const`, `let` or `class`, and code a bundler
wrapped in a closure, are not visible to gojsop; the script then fails with
`missing required export: handle()`.

**Missing fields are normal.** Kubernetes leaves out empty fields: an object
without labels has no `metadata.labels`, and `req.object` is `null` on
DELETE (`req.oldObject` on CREATE). `obj.metadata.labels.team` then throws,
and the call fails. Reach into objects with `?.` and `??`:
`obj.metadata?.labels?.team ?? "none"`.

**No state between calls.** gojsop runs the top-level code once and takes a
snapshot. Every call starts from that snapshot, so globals you change inside
`handle()` are gone at the next call. Keep state in the cluster.

**Reaching the cluster.** The global `kube` takes one object per call:

| Function | Hooks | Policies | Does |
|---|---|---|---|
| `kube.get({apiVersion, kind, name, namespace})` | yes | yes | the object, or `null` when it is missing |
| `kube.list({apiVersion, kind, namespace, labelSelector, fieldSelector})` | yes | yes | the items; an empty `namespace` lists all namespaces |
| `kube.apply(object)` | yes | — | creates the object, or sends a JSON merge patch (not server-side apply); needs `get`, `create` and `patch` |
| `kube.delete({apiVersion, kind, name, namespace})` | yes | — | deletes; a missing object is no error |

A failed call throws, and the script may catch it. Every call needs the
matching right in `spec.permissions` (see [Permissions](#permissions)).
Policies can only read: admission must not change the cluster.

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

A ConfigMap edit reaches the script without a restart.

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

## How gojsop compares

| | gojsop | [jsPolicy](https://github.com/loft-sh/jspolicy) | [Kyverno](https://kyverno.io) |
|---|---|---|---|
| Policies are written in | JavaScript | JavaScript or TypeScript, npm packages | YAML with JMESPath or CEL |
| Engine | QuickJS as WebAssembly, pure Go | V8 through cgo | Go |
| Validate and mutate requests | yes | yes | yes |
| React to changes with code | yes (`JSHook`) | yes (controller policies) | declarative `generate` and `mutate-existing` |
| Audit and warn before enforcing | yes (`enforcement`) | yes (`violationPolicy`) | yes |
| Rights of a script | its own ServiceAccount with the rights it declares; nobody grants more than they hold | the operator is `cluster-admin` | per controller, extended by aggregated roles |
| Policies per namespace | no, cluster-wide only | no | yes |
| Image signature checks | no | no | yes |
| Policy reports and background scans | no, violations are events | violations CRD | yes |
| TypeScript, local test CLI | not yet | TypeScript yes | test CLI yes |
| Status | alpha | last release 2023 | stable, CNCF |

gojsop is for teams that want the freedom of real code, with each script
limited to exactly what it may touch. When a declarative policy language and
a large ecosystem matter more, use Kyverno.

## Known limitations

- **The operator is powerful.** To give hooks their rights, the operator may
  create any ClusterRole and impersonate the ServiceAccounts of its own
  namespace. Whoever controls the operator's Deployment controls the cluster;
  the scripts do not.
- **Failover gap.** Hooks run on the leader only. After a leader change the
  new leader delivers every object again as an initial `Added`: objects that
  changed in between arrive that way, but a deletion in between is never
  seen.
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

Releases come from [release-please](https://github.com/googleapis/release-please):
it keeps a release PR open that collects the conventional commits on `main`.
Merging it tags the version, and CI publishes the image, the Helm chart and
`install.yaml`. `deploy/chart` holds only `Chart.yaml` and `values.yaml`,
kept by hand; `make chart` generates the templates from `config/`, and CI
and the release job run it.

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
