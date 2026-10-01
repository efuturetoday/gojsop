---
id: <file name without .md>
status: proposed        # proposed | accepted | superseded
entrypoints:            # symbols as pkg.Name or pkg.Type.Method, checked by make check-blocks
  - pkg.Symbol
---

# <Title>

This block describes <what part of the system, in one sentence>.

<Plain prose, 3 to 6 short paragraphs. Start with what the system does, then
the choice that was made and why, then the consequences a reader must know.
Name code by symbol (`pkg.Type.Method`), never by file and line. Link other
blocks instead of repeating them.>

## Rules

- **R1** <One imperative sentence.>
  Why: <one sentence>.
  Gate: `TestName` or `make target`. Or: missing → GATE-n. Or: review only — <why>.

## Rejected

- <Alternative>: <why not, one line>.

## Open

Tracked in [backlog](../backlog.md): <keys>.
