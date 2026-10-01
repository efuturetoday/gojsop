---
name: sdlc-core
description: The method of the agentic SDLC library - goal, layers, vocabulary, areas and aspects, rule IDs, gates, open items and the SDLC check. Use when starting work in a project that uses this library, when asked "what is an aspect/area", "where does this go" or "how do we work here", and when setting up `.agents/`.
---

# Agentic SDLC: concept

Status: draft. Goal, layers, vocabulary, the cut between areas and
aspects, rule IDs, both templates, gates and open items are decided. Everything else comes in later steps.

## Goal

One skill library that lets agents and people build and run software the same
way across projects and tech stacks.

## Three layers

| Layer | What it holds | Example |
|---|---|---|
| **Method** | How we work, independent of stack and tracker. Uses slots where a project must fill in a detail. | How to write an aspect; how to cut a story into steps. |
| **Binding** | How the method maps onto one stack or one tool. Reusable across projects. | Go: how symbols are resolved and which gates exist. GitHub: how a story becomes an issue. |
| **Project** | What is true for one repository. Lives in that repository, never in the library. | The project's areas and aspects, its backlog, its gate command. |

A project picks the bindings it needs and fills the remaining slots in one
binding file. The library holds the first two layers only.

## Vocabulary

- **Area**: what the system promises its users in one field: use cases, the
  rules of the domain, and the decisions only this field needs. Who the user
  is depends on the project: a customer in a shop, a clerk in a back office,
  or a developer who uses a platform.
- **Aspect**: one way of doing something that at least two areas share and
  that must be done the same way everywhere, such as how texts are
  translated or how a user signs in. It records the decisions taken, the
  rules that follow and the gates that hold them.
- **Rule**: one imperative sentence inside an aspect, with its reason and its
  gate.
- **Gate**: the check that fails when a rule is broken: a test, a lint rule, a
  CI job. A rule no machine can check says so ("review only").
- **Binding**: a skill or file that maps the method onto a stack, a tool or a
  project.
- **Slot**: a named gap in a method skill, written `{name}`, that a binding
  fills.

## Layout in a project

Fixed paths, the same in every project:

| Path | Holds |
|---|---|
| `.agents/sdlc.md` | the binding file |
| `.agents/areas/<id>.md` | one file per area |
| `.agents/aspects/<id>.md` | one file per aspect |

## The binding file

`.agents/sdlc.md`, from `binding-template.md` (next to this file). Its front matter holds only
what differs between projects and cannot be read from elsewhere:

| Slot | What |
|---|---|
| `gate` | the command that runs every gate, for example `make test lint`; skills run it before a change is done |

Which bindings a project uses is not written here: it is what is installed
(`.agents/skills/`, `skills-lock.json`). A project installs exactly one
tracker binding; the SDLC check fails on none or two.
Bindings are named `sdlc-tracker-<name>` and `sdlc-stack-<name>`, so the
check can tell them apart.

The file has no body. Its presence also marks that the project uses the
library. A slot that is not filled is a gap; a skill stops and names it
instead of guessing; the SDLC check reports it. A new slot is added only when a skill or the SDLC
check needs one and no fixed convention can do the job.

## Areas and aspects

Areas run lengthwise: each is one field of the domain. Aspects run across:
each is used by several areas.

```
               Area: Orders   Area: Invoices   Area: Returns
Aspect: i18n         ●              ●               ●
Aspect: auth         ●              ●               ●
Aspect: logging      ●              ●               ●
```

Where a piece of knowledge goes:

| It is … | It goes to |
|---|---|
| behaviour the user sees, a use case, a domain rule | the **area** |
| a technical choice only one area needs | the **area**, as a decision |
| a way of doing something that two or more areas share, must be done one way, and can be held by a rule with a gate | an **aspect** |
| a choice made once that shapes everything (stack, topology) | a decision in the aspect it shapes |

Modules (packages, libraries, services) are implementation, not a unit of
this concept. An area or an aspect names the modules it uses by symbol. One
module often serves one area, but it does not have to.

## Rule IDs

Every rule has an ID of the form `<id>.R<n>`, where `<id>` is the area or
aspect it belongs to (`orders.R3`, `i18n.R1`). Use cases are `<id>.UC<n>`.
IDs are never reused or renumbered; a dropped rule is marked withdrawn.

Code and tests point back to a rule with a comment that holds the ID, for
example `// i18n.R1` above the code that implements it or the test that holds
it. One syntax everywhere, so one checker finds every link.

## Gates

A **gate** checks that the code keeps a rule of an area or aspect: a test, a
lint rule, an architecture test, a CI job. Gates belong to the project and
are written with the tools of its stack. Every rule says how it is held:

- **A rule of an area** is held by a test, or it is `missing → <key>`. Domain
  rules describe behaviour, and behaviour can be tested.
- **A rule of an aspect** is held by a gate, it is `missing → <key>`, or it is
  `review only — <reason>` when no machine can check it reliably (for
  example: no abstraction for a single case; no secrets in source text).
- **An accepted aspect** has at least one gate a machine runs. Without one it
  stays `proposed`.
- **No exception lists.** Code that breaks a new gate is fixed in the same
  step, never exempted.

## SDLC check

The **SDLC check** checks that a project keeps this method. It ships with
the library, works the same in every project, and knows nothing about the
domain. It runs in CI next to the gates, as a step of its own.

It checks that:

0. `.agents/sdlc.md` exists and fills `gate`, and exactly one tracker
   binding is installed;
1. every rule has a gate line in an allowed form;
2. no rule of an area is `review only`;
3. every accepted aspect has at least one rule held by a gate a machine runs;
4. every key in `missing → <key>` exists in the tracker;
5. every test a rule names exists and carries the rule's ID in a comment;
6. every ID in a comment in code or tests points to a rule that exists.

The check has a **core** and **ports**. The core reads only the Markdown
files and the binding file: format, IDs, gate lines, and the rules above that
need nothing else (0, 1, 2, 3). Anything that needs the code or the tracker
goes through a port, with an adapter from a binding:

| Port | Answers | Adapters, for example |
|---|---|---|
| symbols | does `pkg.Type.Method` exist? | go, ts, java |
| references | where do comments `<id>.R<n>` appear, and which are tests? | go, ts, java |
| tracker | does key `AUTH-3` or `#123` exist? | file-backlog, github |

A check whose adapter is missing is reported as not run; it is never
skipped silently.

It does not run the gates, and it cannot tell whether a gate really fails
when its rule is broken. That stays a review: a gate nobody has seen fail is
a claim, not a gate.

## Open items

Something missing, broken or undecided is an **open item**. The method
defines what an open item is; the binding decides where it is stored.

Every open item has:

- a stable **key**, never reused,
- a **type**: `bug`, `gap`, `gate`, `debt`, `doc` or `decision`,
- **evidence**: what shows the problem,
- **done when**: what is true once it is closed.

It lives in exactly one place. The `Open` section of an area or aspect lists
only the keys of its items. A commit that closes an item names its key.

Where items are stored and what a key looks like is decided by the tracker
binding: it says where items live and what a key looks like, for example
`AUTH-3` or `#123`. A project can start with one tracker and move to another
later without changing the method.

## Skills in this library

| Skill | Use it when |
|---|---|
| sdlc-core | You need the method: vocabulary, layout of `.agents/`, rule IDs, gates, open items. Start here. |
| sdlc-area | You create or audit an area file in `.agents/areas/`. |
| sdlc-aspect | You create or audit an aspect file in `.agents/aspects/`. |
| sdlc-stack-go | A Go project: how symbols and rule anchors are written; activates the Go adapter of the SDLC check. |
| sdlc-tracker-file-backlog | Open items as a file in the repository: entry format and keys; activates the file-backlog adapter. |
| caveman | You want terse, token-saving answers. |
| caveman-commit | You write a Conventional Commits message. |

Binding skills follow the naming rule already given under "The binding
file": `sdlc-tracker-<name>` and `sdlc-stack-<name>`.

## Adapters

Adapters are compiled into `sdlc-check`. An adapter is active when its
binding skill is installed in the project (`.agents/skills/<name>/`):
`sdlc-stack-go` activates the Go adapter for symbols and references,
`sdlc-tracker-file-backlog` the file-backlog adapter for the tracker. An
installed binding with no adapter in the binary is reported as a problem
("unknown binding").

## Running the SDLC check

    go run github.com/efuturetoday/agentic-sdlc/cmd/sdlc-check@latest [repo-root]

Needs Go. `repo-root` defaults to `.`. It prints one line per problem as
`path:line: message`. Exit code 0 means clean, 1 means problems, 2 means an
internal error. A check whose port has no adapter is reported as
`not run: <port> (no adapter installed)`; that is a notice and does not
change the exit code.
