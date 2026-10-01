---
id: <file name without .md>
status: proposed          # proposed | accepted | superseded
entrypoints:              # symbols a reader opens first; checked by the gate
  - <pkg.Symbol>
---

# <Aspect>

This aspect describes <what part of the system, in one sentence>.

<Plain prose, 3 to 6 short paragraphs: what the system does here, the choice
that was made and why, the consequences a reader must know. Name code by
symbol, never by file and line. Link other aspects instead of repeating them.>

## Parts

<Every row needs an answer: where it is, "planned", or "n/a, because …".
Never delete a row. An aspect with no building block at all is fine; then
most rows say "n/a" with the reason.>

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once, so nobody builds it a second time? A library, a framework, a service, a component, a builder (for example a form builder), a code generator. | |
| example | Which real use in the code should others copy? | |
| test helper | How does a test use the aspect without effort? A fake, a harness, a fixture, a provider, a test container. | |
| sides | Which sides does it touch (back end, front end, mobile, CLI, infrastructure)? | |
| tie | If it touches more than one side: how do the sides stay in step? Generated from one side, or checked by a test. Same spelling on both sides is not a tie. | |

## How to use it

<The change people make most often (add a word, add a field, add a flag), as
numbered steps, ending with the gate. "n/a, because …" if there is none.>

## Rules

- **R1** <One imperative sentence.>
  Why: <one sentence>.
  Gate: `<test or command>` | missing → <KEY> | review only — <why>.

The full ID of a rule is `<id>.R<n>`. Code that implements it and tests that
hold it carry the ID in a comment, for example `// <id>.R1`.

## Decisions

- **<The decision, one sentence.>** Status: accepted (<date>, <who>).
  Why: <reasons>. Not taken: <option>, because <reason>.

## Open

<Keys of the open items of this aspect, one per line. The items themselves
live where the tracker binding keeps them.>
