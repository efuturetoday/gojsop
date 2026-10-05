# Contributing to gojsop

## Make targets

```sh
make test          # unit and envtest integration tests
make lint          # golangci-lint
make test-e2e      # e2e tests on a throwaway kind cluster
make engine-wasm   # rebuild the embedded QuickJS engine (after glue.c changes)
make sdk-test      # vitest suite of the npm packages against bin/gojsop
make sdk-smoke     # pack the npm packages, create a workspace, run its tests
make help          # every target
```

## Releases

Releases come from [release-please](https://github.com/googleapis/release-please):
it keeps a release PR open that collects the conventional commits on `main`.
Merging it tags the version, and CI publishes the image, the Helm chart,
`install.yaml`, the CLI binaries and the npm packages in [`sdk/`](sdk). The
npm packages take their version from the tag; the repository keeps `0.0.0`.

The npm packages are published with
[trusted publishing](https://docs.npmjs.com/trusted-publishers/): no token,
npm trusts `release.yml` through OIDC. A new package needs this once, by a
maintainer:

`npm login`, then `make sdk-claim` in a terminal. It publishes a
placeholder `0.0.0` of every package that does not exist yet, because
trusted publishing works only for existing packages, and trusts
`release.yml` of this repository to publish each package. npm asks for 2FA.

## Helm chart

`deploy/chart` holds only `Chart.yaml` and `values.yaml`, kept by hand.
`make chart` generates the templates from `config/`; CI and the release job
run it, so the chart always matches `config/`.

## Project rules

How the project is organised, its rules and its open items live in
[`.agents/`](.agents/README.md); [AGENTS.md](AGENTS.md) is the entry point.

`make sdlc-check` checks `.agents/` against the code. It runs a tool from the
private repository `github.com/efuturetoday/agentic-sdlc`; the target sets
`GOPRIVATE`, and Git needs credentials for GitHub, for example:

```sh
git config --global credential.https://github.com.helper '!gh auth git-credential'
```
