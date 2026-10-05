# gojsop reference

Every field of `JSHook` and `JSAdmission`, the rights of a script, what a
script can call, and the status a resource shows. The [README](../README.md)
walks you through a first hook and policy.

- [Install](#install)
- [JSHook](#jshook)
- [JSAdmission](#jsadmission)
- [Permissions](#permissions)
- [Scripts](#scripts)
- [Status](#status)
- [Workspace and CLI](#workspace-and-cli)

## Install

gojsop needs [cert-manager](https://cert-manager.io), which issues the webhook
certificate:

```bash
helm repo add jetstack https://charts.jetstack.io
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace --set crds.enabled=true --wait
```

With Helm:

<!-- x-release-please-start-version -->
```bash
helm install gojsop oci://ghcr.io/efuturetoday/charts/gojsop \
  --namespace gojsop-system --create-namespace --wait \
  --version 0.1.1
```
<!-- x-release-please-end -->

Or without Helm:

<!-- x-release-please-start-version -->
```bash
kubectl apply -f https://github.com/efuturetoday/gojsop/releases/download/v0.1.1/install.yaml
```
<!-- x-release-please-end -->

The samples, a ConfigMap sync hook and two pod policies:

<!-- x-release-please-start-version -->
```bash
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


## Scripts

**Entry points are global functions.** The operator runs one JavaScript
script and calls `validate`, `mutate` or `handle` as a global function. In a
workspace you write TypeScript with `export function handle`; `gojsop build`
bundles it and makes the exports global. In a plain script, write
`function handle() {}` (or `var handle = ...`): a top-level `const`, `let` or
`class` is not visible to gojsop, and the script fails with
`missing required export: handle()`. An entry point must not be `async`.

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
policy (`kubectl describe jshook <name>`). In a test they are in
`result.console`.

**Limits.** `spec.limits.memoryMB` (default 32, at most 512) and
`spec.limits.timeoutSeconds` (default 30, at most 300) apply to every call
and to loading the script. A script stuck in a loop is stopped at the
deadline; `try/catch` cannot hold it.

**Sources.** Inline in `spec.source.inline` (what `gojsop build` writes), or from a ConfigMap:

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

## Workspace and CLI

`npm create @gojsop <dir>` creates a workspace: one directory per hook or
policy, holding `policy.yaml` or `hook.yaml` (without `spec.source`), the
script `policy.ts` or `hook.ts` (or a plain `.js`) and its tests.

| Command | Does |
|---|---|
| `npx gojsop new <policy\|hook> <name>` | creates `policies/<name>` or `hooks/<name>` with manifest, script and a passing test; a new policy starts with `enforcement: Audit` |
| `npx gojsop rn <name> <new-name>` | renames `metadata.name` and the directory |
| `npx gojsop rm <name> [--yes]` | removes the directory, after a confirmation; the resource stays in the cluster |
| `npx gojsop build` | writes `dist/policy-<name>.yaml` and `dist/hook-<name>.yaml` with the bundled script in `spec.source.inline` (at most 512 KiB), and removes files of hooks and policies that are gone |
| `npx gojsop run <manifest> (--request <file> \| --event <file>) [--cluster <file>] [--trace]` | runs one call against a cluster in memory and prints the result; `--trace` prints every `kube.*` call |

A name is a DNS label: lowercase letters, digits and `-`, at most 63
characters. A name that a hook and a policy share needs the directory
instead, for example `gojsop rm hooks/<name>`.

The input of a call, in a test (`review("./pod.yaml")`) or with
`--request` / `--event`, is a YAML or JSON file with a request or an event,
or a plain object as `kubectl get -o yaml` prints it; a plain object becomes
the request's or event's `object`.

Tests use [`@gojsop/testing`](../sdk/testing/README.md): `policy(path).review()`,
`hook(path).handle()`, a cluster in memory and fixtures. A script runs in the
operator's engine, never in Node.
