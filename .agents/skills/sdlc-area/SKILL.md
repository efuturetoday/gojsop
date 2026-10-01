---
name: sdlc-area
description: Write or audit an area - what the system promises its users in one field of the domain - as use cases and domain rules with IDs, each rule held by a test. Records it in .agents/areas/<id>.md. Use for "new area", "describe the domain of", "write the use cases", "which rules have no test", "audit area".
---

# sdlc-area

    /sdlc-area new <id>
    /sdlc-area audit [<id>]

Run the SDLC check first and fix what it reports before you start.

Load the project's bindings too: the stack binding (`sdlc-stack-<name>`) says
how to name symbols and anchor rules in tests, the tracker binding
(`sdlc-tracker-<name>`) how to open an item for `missing → <key>`.

An area lives in `.agents/areas/<id>.md`, in the shape of `template.md` next
to this file. It describes what users can do and what rules hold, never how
the code does it. The agent does the legwork; a human decides.

## Mode: new

1. **Name the users.** Who uses this field, and what they want to achieve.
   Ask if the sources do not say.
2. **Collect the sources.** Requirements, documents, tickets, the code of a
   legacy system, people's answers. Every rule needs one. A rule without a
   source is an open question: create a `decision` item in the tracker and
   use its key; never guess the rule.
3. **Write the use cases.** One per thing a person does, with ID
   `<id>.UC<n>`: actor, trigger, what must hold before, steps, exceptions,
   result.
4. **Write the rules.** One line each, ID `<id>.R<n>`, with its source. Hold
   each by a test that carries the ID in a comment (`// <id>.R<n>`), or write
   `missing → <key>` after you created the open item. There is no
   "review only" for a domain rule.
5. **Link aspects, not their rules.** List the aspects this area uses. A
   technical choice that only this area needs goes under Decisions; one that
   other areas share belongs to an aspect (`sdlc-aspect`).
6. **Run the SDLC check and the project's gate** (`gate` in
   `.agents/sdlc.md`). Both pass before you hand over.
7. **Hand over** with the open questions. Only a human sets
   `status: accepted`, with date and name.

## Mode: audit

Read only. For each area, or the one named:

1. Every rule has a source.
2. Every finding of the SDLC check is a gap.
3. Every use case is reachable in the system as described.
4. The text describes behaviour, not implementation. Implementation detail
   is a gap.
5. A technical choice two areas share and that sits in one area's
   Decisions is a candidate aspect.

Report one line per gap: area, use case or rule, evidence, what done looks
like. Record each gap as an open item in the tracker and list its key in the
area's Open section. Change nothing else.

## What this skill does not do

- Invent a rule, a source or an answer.
- Describe how the code works; name the aspects instead.
- Set `accepted`.
