---
name: write-block
description: Write or update a block in .agents/blocks (one documented decision with rules and gates). Use when creating a new block, migrating a block to the current format, or changing a decision, rule or gate.
---

# Write a block

A block documents one facet of gojsop: the decision taken, why, the rules that
follow from it, and the gates that enforce those rules. People read blocks
first, agents second. Write for a developer who is new to the project.

The reference block is `.agents/blocks/js-execution.md`. The empty skeleton is
`.agents/blocks/_template.md`.

## Structure

1. **Front matter.** `id` (equals the file name without `.md`), `status`
   (`proposed`, `accepted` or `superseded`), `entrypoints` (the few symbols a
   reader should open first, as `pkg.Name` or `pkg.Type.Method`).
2. **Title** `# <Name>`.
3. **Introduction.** The first sentence says what the block covers:
   "This block describes how gojsop ...". Then 3 to 6 short paragraphs of
   plain prose:
   - what the system does in this area,
   - the choice that was made and why,
   - the consequences a reader must know (limits, defaults, trade-offs).
   If the reason for a choice is not recorded, say so in one sentence. Never
   invent a reason.
4. **`## Rules`.** Each rule:
   ```
   - **R1** <one imperative sentence>.
     Why: <one sentence>.
     Gate: `TestName`, `make target`, or missing → GATE-n.
   ```
   A missing gate always names a backlog key. If no `GATE-n` fits, add a new
   one to the backlog; never borrow an unrelated key. If a machine cannot
   check the rule, write `Gate: review only — <why>`. Never drop a rule to
   avoid a gate.
   Number rules R1..Rn per block. Never renumber; mark a dropped rule
   `(withdrawn)` instead. A violated rule adds "Violated today → <key>".
5. **`## Rejected`.** Alternatives that were considered, one line each with
   the reason. Leave the section out when there are none.
6. **`## Open`.** One line: "Tracked in [backlog](../backlog.md): <keys>."
   Findings, bugs and gaps go into `.agents/backlog.md`, never into the block.

## Writing rules

- Name code by symbol (`jsregistry.Registry.Call`), never by file and line.
  Line numbers drift; symbols are checked.
- Do not describe the code inventory (every field, lock, caller). gopls and
  graft answer that. Keep only what a reader needs to understand the decision.
  Put details about one symbol in its Go doc comment.
- Link other blocks instead of repeating them.
- English, short sentences, active voice, one idea per sentence. One term per
  concept. No filler, no hedging, no notes about how the block was researched.
- Target 40 to 80 lines.

## Code anchors

Mark the code that implements a rule with a doc-comment line:

```go
// Block: js-execution R4
func wrapEngineErr(ctx context.Context, err error) error {
```

Add only comment lines. An anchor makes the link visible from the code side:
whoever changes that symbol sees which rule depends on it.

## Gate

Run `make check-blocks` before you finish. It parses the Go code with
`go/parser` and fails when:

- an entrypoint or a backticked `pkg.Symbol` does not exist,
- a backticked `TestName` does not exist (lines that contain "missing" are
  skipped; `TestFoo_*` matches a prefix),
- a rule has no `Gate:` line,
- a `// Block: <id> <rule>` anchor names an unknown block or rule.

Blocks without front matter are not checked.

## Changing a decision

Update the block in the same change as the code. Set `status: superseded`
and link the new block, or rewrite the rule and its Why. Never leave a block
that contradicts the code.
