# Project knowledge for agents and people

Project knowledge lives here, one Markdown file per aspect. We call each file a
**block**. A block records one decision and ties it to four things:

- **Decision**: what holds and why.
- **Code**: the API that implements it, the single allowed entry point.
- **Rules**: what to do and what not to do when working on it.
- **Gates**: tests, lint rules or CI jobs that enforce the decision.

A block without a gate is an intention. A gate marked `missing` in a block is a
commitment to add it.

## Working with blocks

1. Before you change code, find the blocks for the area you touch and read them.
2. Follow their rules. If the change breaks a decision, update the block in the
   same change (status `superseded` or a new decision), never silently.
3. When you add a gate, change its row from `missing` to the real command.
4. New facet: copy `blocks/_template.md`, keep it under ~120 lines, cite code
   as `path:line`.
5. Open items live in [backlog.md](backlog.md) with stable keys. A block's
   `Open` section only lists the keys.

## Blocks

| Block | Status | Scope |
|-------|--------|-------|
| [js-execution](blocks/js-execution.md) | accepted | QuickJS on wazero via qjs, VM lifecycle, ctx cancellation, limits, host APIs, isolation |
| [js-registry](blocks/js-registry.md) | accepted | One VM per resource key, `Registry.Call`, build and restart, locking |
| [status-conditions](blocks/status-conditions.md) | accepted | CRD status fields, `Ready` condition, restart bookkeeping, who writes status |
| [js-sources](blocks/js-sources.md) | accepted | `spec.source`, loaders, ConfigMap watch, source hash |
| [hook-dispatch](blocks/hook-dispatch.md) | accepted | `config()` bindings, informers, queue, BindingContext, `handle()` |
| [admission-webhook](blocks/admission-webhook.md) | accepted | Webhook registration, `review`, `validate()`, patches, TLS |
| [kube-access](blocks/kube-access.md) | accepted | `kubehost.Factory`, `kube.*` semantics, RBAC |
| [api-design](blocks/api-design.md) | accepted | CRD markers, validation, naming, versioning |
| [kubebuilder-scaffold](blocks/kubebuilder-scaffold.md) | accepted | Generated files, scaffold markers, layout deviation |
| [testing](blocks/testing.md) | accepted | Test layers, Make targets, CI workflows, coverage |
