---
name: sdlc-stack-go
description: How a Go project links code and tests to the rules of its areas and aspects - how to name Go symbols in .agents files, how to anchor a rule in a Go test or in code with a comment, which functions count as tests. Load it before you write or change a Go test that holds a rule, add a gate in Go, name a Go symbol in an area or aspect, or fix an sdlc-check finding about symbols, references or anchors. Installing it also switches on the Go adapter of sdlc-check.
---

# sdlc-stack-go

Installing this skill (directory `.agents/skills/sdlc-stack-go/`) switches on
the Go adapter built into `sdlc-check`. It parses every `.go` file of the
project, except `.git`, `vendor`, `bin`, `testdata`, `node_modules` and hidden
directories, and answers the symbols and references ports. Without it those
checks report `not run`.

## Naming symbols

Use the package name from the `package` clause, not the directory or import
path, and without a `_test` suffix.

| Symbol | Written as |
|---|---|
| function, type, var, const | `pkg.Name` |
| method, pointer or generic receiver | `pkg.Type.Method` |
| struct field | `pkg.Type.Field` |
| interface method | `pkg.Type.Method` |

Put symbols in backticks in `entrypoints:` and in the Parts table of an aspect.

## Naming tests

A gate names a test by its bare name in backticks: `TestNoRawTexts`. Tests are
functions named `Test*`, `Benchmark*`, `Example*` or `Fuzz*` in a `_test.go`
file. `TestCancel_*` stands for every test with that prefix.

## Rule anchors

A comment holding the rule ID points back to the rule: `// orders.R3`. For a
test, put it in the doc comment above the test function or inside its body.
Every test a gate names must carry the ID of that rule this way. The ID is
`<id>.R<n>` with `<id>` in lowercase letters, digits and dashes; an ID in any
comment that matches no rule in an area or aspect is reported as dangling.
