---
id: npm-packages
status: proposed
entrypoints: []
---

# npm packages

This aspect describes how the npm packages in `sdk/` are written, checked,
versioned and published: `@gojsop/cli` with its platform packages,
`@gojsop/types`, `@gojsop/testing` and `@gojsop/create`. The
[workspace](../areas/workspace.md) area says what they promise users.

`sdk/` is one npm workspace with one lockfile. Every package is TypeScript
in strict mode, built with `tsc` to `dist/`, except `@gojsop/types`, which is
a hand-written `.d.ts`. Biome formats and lints all of it.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once? | `sdk/scripts/publish.ts` builds the platform packages, stamps the version and publishes; `sdk/cli/src/bundle.ts` bundles scripts with Vite for `@gojsop/testing` and `gojsop build`. |
| example | Which real use should others copy? | `sdk/testing/src/client.ts`. |
| test helper | How does a test use the aspect without effort? | `make sdk-test` runs the vitest suite against `bin/gojsop`; `make sdk-smoke` installs the packed packages into a new workspace. |
| sides | Which sides does it touch? | npm (the packages), CI (`test.yml` job `npm packages`, `release.yml`). |
| tie | How do the sides stay in step? | CI calls the same make targets as a developer. |

## How to use it

1. Change code under `sdk/<package>/src`.
2. Run `make sdk-format`, then `make sdk-lint sdk-test`.
3. A change that affects installing or `npm create @gojsop` also runs
   `make sdk-smoke`.

## Rules

- **R1** Format and lint every file in `sdk/` with Biome, with its
  recommended rules; a rule is switched off only in place, with a
  `biome-ignore` comment that gives the reason.
  Why: one style without debate, and mistakes such as `any` found before
  review.
  Gate: `make sdk-lint`.
- **R2** Write the packages in strict TypeScript and build them with `tsc`.
  Why: users get types, and type errors fail the build.
  Gate: `make sdk-test`.
- **R3** Keep `0.0.0` as the version of every package in the repository; the
  release job stamps the tag's version and pins the packages to each other.
  Why: one source of the version; release-please touches no `package.json`
  and no lockfile.
  Gate: review only — `publish.ts` stamps every package it publishes, but no
  check reads the versions in the repository.

## Decisions

- **Biome formats and lints the npm packages.** Status: accepted
  (2026-10-05). Why: one fast tool for both, one config file, no plugins to
  keep in step. Not taken: ESLint with Prettier, because two tools and their
  plugin configs cost upkeep for four small packages.
- **Scripts are bundled with the project's Vite, not with a bundler of our
  own.** Status: accepted (2026-10-05). Why: vitest already brings Vite, so
  users install nothing more, and test and deploy bundle the same way. The
  operator never bundles; it takes one JavaScript file. Not taken: esbuild as
  a dependency of `@gojsop/cli`, because it would be a second bundler next
  to Vite.

## Open

None.
