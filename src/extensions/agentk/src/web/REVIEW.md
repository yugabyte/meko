# REVIEW.md: the web UI

What to check on changes under `src/web/`, on top of Kode's [REVIEW.md](../../REVIEW.md). The
rules behind these checks are in [AGENTS.md](AGENTS.md). A UI change that breaks the phone layout is
a **major** finding.

## Phone and touch

- [ ] The change works at 390 px wide: nothing is cut off, overlaps, or makes the page scroll
      sideways (code, diffs and wide tables scroll inside their own box).
- [ ] New controls are reachable on a phone. A new session action is an entry in `sessionActions()`
      (`src/web/sessionactions.ts`), which the desktop ⋯ menu, the ⋯ sheet and the palette read,
      not a button of its own in the stage bar (which keeps the gate and what needs the user) or a
      row only one of them has. Footer additions appear in the ⋯ sheet unless the phone footer shows
      them (like the diff size, which opens Changes); project-level actions appear in the drawer.
- [ ] Nothing needs hover, right-click, modifier-click, drag and drop, or a keyboard shortcut
      without a tap equivalent. Needed information isn't only in a `title` tooltip.
- [ ] Touch targets are at least 36 px tall (40 px for icon buttons) under `(pointer: coarse)`.
      The one exception is the rail's code review box (`.rail-review`), kept 13 px tall so a
      task's row stays short.
- [ ] A setting (Settings, Project settings) shows at most one short line of hint, about 50
      characters; a longer explanation goes behind an ⓘ (`InfoButton` and `InfoPanel`,
      `InfoCheck` or `Field`'s `info` in `common.tsx`), whose text opens under the whole setting.
      Status and what to do about it (versions, warnings, errors) stay inline.
- [ ] New text inputs are 16 px or larger in the phone section of `styles.css`.
- [ ] Full-height views use `var(--app-h, 100dvh)` on phones, not `100vh`. Bars at the screen edges
      respect `env(safe-area-inset-*)`. Full-screen fixed layers (overlays) sit at
      `var(--app-top)` with `var(--app-h)`, not `inset: 0`, so the keyboard doesn't cover them.
- [ ] Phone-only behavior uses `MOBILE_QUERY` / `TOUCH_QUERY` from `hooks/useMedia.ts` and the
      matching CSS media queries, not new ad-hoc breakpoints or user-agent sniffing.
- [ ] The desktop layout is unchanged unless the change is meant for it.
- [ ] Touch input to the terminal goes through `terminals.ts` / `keys.ts`, and `keys.ts` changes
      have tests. The terminal doesn't take focus on touch screens outside typing mode.

## Reloads after an update

- [ ] A new place to type that keeps its text across a reload (in `sessionStorage`, say) is marked
      `data-reload-safe`. Unsent text in any other field is in a `textarea`, a dialog or storage,
      so the automatic reload after an update (`reload.ts`) waits for it instead of losing it.
