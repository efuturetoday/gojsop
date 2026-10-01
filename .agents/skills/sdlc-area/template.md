---
id: <file name without .md>
status: proposed          # proposed | accepted | superseded
---

# <Area>

This area describes what <users> can do with <field of the domain>.

<Plain prose, 2 to 4 short paragraphs: who the users are, what they want to
achieve here, how this area relates to its neighbours. No implementation.>

## Use cases

### <id>.UC1 <What a person does, as a verb phrase>

- **Actor**: <who>
- **Trigger**: <what starts it>
- **Before**: <what must hold, e.g. a permission, a setting>
- **Steps**: <numbered; what the person does and what the system answers>
- **Exceptions**: <what can go wrong; point to rules or other use cases>
- **Result**: <what is true afterwards>

## Rules

<The domain rules this area must keep. One line each. Every rule has a
source; a rule without a source is an open question, never a guess.>

| ID | Rule | Source | Held by |
|---|---|---|---|
| <id>.R1 | <one sentence> | <requirement, document, legacy code, decision> | `<test name>` or missing → <KEY> |

Every rule is held by a test or is `missing → <KEY>`. There is no
"review only" for domain rules.

A test that holds a rule names its ID in the comment above it, for example
`// <id>.R1`.

## Aspects

<The aspects this area uses, one line each with a link. Not their rules.>

## Decisions

<Technical choices only this area needs. Choices two or more areas share
belong to an aspect.>

- **<The decision, one sentence.>** Status: accepted (<date>, <who>).
  Why: <reasons>. Not taken: <option>, because <reason>.

## Open

<Keys of the open items of this area, one per line. The items themselves
live where the tracker binding keeps them.>
