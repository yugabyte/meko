# REVIEW.md: Present

What to check in a change to `src/extensions/present`, beyond the repo's rules.

1. **Access.** Does anything listen on more than the socket? Does every new write route or
   WebSocket go through the same-origin check (it keys on the matched route)? Does every new URL the
   pages build go under the `<base href>` (`appUrl`, `wsUrl`)?
2. **The deck's rules.** Does every change to a deck go through `applyOps` and `deckProblems`? A new
   field on a scene, edge, slide or block needs a check there and a row in `test/unit/deck.test.ts`.
3. **Claude.** Does Claude's output reach the deck only as checked operations? Are files it names
   read only from the deck's folder? Is Meko's key kept off its command line? Does a new kind of job
   say what it's doing (activity) and end with a message a person can act on?
4. **Hand edits.** Can anything Claude does overwrite a hand-edited slide without a change set?
5. **Storage.** Is every artifact Present writes at most `PART_BYTES`? Is `deck.json` still the
   last thing a save writes? Is every fetched piece checked against its hash? A change to the stored
   form needs a new `STORE_VERSION` and a way to read the old one.
6. **Rendering.** Is everything that comes from a deck rendered as structured blocks, with Markdown
   and SVG through DOMPurify and Mermaid in strict mode? No HTML or script from a datapack.
7. **Text.** Plain, active, specific; errors say what to do.
8. **Tests.** One test per behavior, extended rather than duplicated; nothing touches the real Meko,
   Claude Code or home.
9. **Docs.** AGENTS.md, README.md and docs/design.md describe what the code now does.
10. **Nothing personal.** Is the diff, the PR title and its description free of internal hostnames,
    paths, tokens and personal data (a real person's name, email or handle, customer names, real
    deck or datapack content, unscrubbed logs or screenshots)? Tests and docs use made-up values.
    The description's Meko lines from AgentK's PR prompt are allowed and stay as written.
