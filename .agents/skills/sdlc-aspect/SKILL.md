---
name: sdlc-aspect
description: Write or audit an aspect - one way of doing something that several areas share and that must be done the same way everywhere (translations, sign-in, feature flags, forms, logging, a test harness). Records its parts, rules, gates and decisions in .agents/aspects/<id>.md. Use for "new aspect", "write an aspect", "audit aspect", "which aspect lacks a gate", or before building something every feature will need.
---

# sdlc-aspect

    /sdlc-aspect new <id>
    /sdlc-aspect audit [<id>]

Run the SDLC check first and fix what it reports before you start.

Load the project's bindings too: the stack binding (`sdlc-stack-<name>`) says
how to name symbols and anchor rules in tests, the tracker binding
(`sdlc-tracker-<name>`) how to open an item for `missing → <key>`.

An aspect lives in `.agents/aspects/<id>.md`, in the shape of `template.md`
next to this file. The agent does the legwork; a human decides.

## Is it an aspect?

Answer three questions before you write anything:

1. Do at least two areas need it?
2. Must it be done one way everywhere, because it drifts otherwise?
3. Can at least one rule about it be held by a gate?

Three times yes: an aspect. Behaviour the user sees: an area
(`sdlc-area`). Needed by one area only: a decision in that area. Say which
it is and stop if it is not an aspect.

## Mode: new

1. **Find what exists.** Search the code before you answer any row of the
   Parts table. Look for:
   - a block: a library, framework, service, component, builder or code
     generator that already does the work, and any second way that does the
     same around it;
   - an example: one real use worth copying;
   - a test helper: fakes, harnesses, fixtures, providers, test containers;
   - sides: every part of the system it touches;
   - a tie: how those sides stay in step (generated, or checked by a test).

   Write "n/a, because …" only after the search found nothing, and say what
   you searched.
2. **Bring options where a choice is open.** Two or three, each with its
   cost and evidence (current library docs, not memory; what the code does
   today). Recommend one. Do not choose.
3. **Write the file** from `template.md` with `status: proposed`. The
   introduction starts with "This aspect describes …". Name code by symbol,
   never by file and line.
4. **Write the rules.** Each rule is one imperative sentence with Why and
   Gate. The gate is a test or command that exists, `missing → <key>`, or
   `review only — <reason>`. For every `missing`, create the open item in the
   tracker first and use its key. Never borrow an unrelated key. Never drop a
   rule to avoid a gate.
5. **Link code and tests.** Put the rule ID in a comment above the code that
   implements a rule and above each test that holds it: `// <id>.R<n>`.
   Comment lines only.
6. **Run the SDLC check and the project's gate** (`gate` in
   `.agents/sdlc.md`). Both pass before you hand over.
7. **Hand over.** List the open questions and the choices waiting for a
   human. Only a human sets `status: accepted`, with date and name. An
   accepted aspect needs at least one rule held by a gate a machine runs.

## Mode: audit

Read only. For each aspect, or the one named:

1. Re-run the search of step 1 of "new". A row that says "n/a" while a block,
   example or helper exists is a gap. So is a second way around the block.
2. Every finding of the SDLC check is a gap.
3. Every gate has been seen to fail. If nobody can say how, report it as a
   claim, not a gate.
4. A rule the code breaks today is reported with its evidence.

Report one line per gap: aspect, rule or part, evidence, what done looks
like. Record each gap as an open item in the tracker and list its key in the
aspect's Open section. Change nothing else.

## What this skill does not do

- Choose an option or set `accepted`.
- Leave a Parts row out, or answer it without a search.
- Add an exception list to a gate.
- State a rule that another aspect already states; link that aspect instead.
