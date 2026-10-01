---
name: sdlc-tracker-file-backlog
description: How open items are kept in .agents/backlog.md - the entry format, how keys are built, how an item is opened and closed. Load it before you write missing → <key> in a rule, record a gap from an audit, open or close an open item, or fix an sdlc-check finding about keys. Installing it also switches on the file-backlog adapter of sdlc-check.
---

# sdlc-tracker-file-backlog

Installing this skill (directory `.agents/skills/sdlc-tracker-file-backlog/`)
makes `.agents/backlog.md` the tracker and switches on the file-backlog
adapter built into `sdlc-check`. The check reads the file and fails when a
key named in `missing → <key>` has no entry, or when keys are named and the
file does not exist. Install exactly one tracker binding.

## Entry format

One entry per item, starting at the beginning of a line:

    - **AUTH-3** `gap` Sign-in has no lockout test. Evidence: login_test.go covers only success. Done when a test shows five failed attempts lock the account.

- The key is `[A-Z][A-Z0-9]*-[0-9]+`. The check finds an item by the line
  start `- **KEY**`.
- Then the type in backticks (`bug`, `gap`, `gate`, `debt`, `doc`,
  `decision`), a short title, the evidence and the done-when condition.

## Keys

- Use one prefix per area or aspect, in capitals: `AUTH-3` for the aspect
  `auth`, `ORD-1` for the area `orders`.
- Count up per prefix. Never reuse a key, not even after the item is gone.
- Only the key goes into the `Open` section of an area or aspect, and into
  `missing → <key>` gates; the item lives only in the backlog.

## Closing

Delete the entry when the item is done and name its key in the commit that
closes it. Do not leave closed entries behind.
