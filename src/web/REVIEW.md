# REVIEW.md: the web UI

What to check on changes under `src/web/`, on top of the root [REVIEW.md](../../REVIEW.md). The
rules behind these checks are in [AGENTS.md](AGENTS.md). A UI change that breaks the phone layout is
a **major** finding.

## Phone and touch

- [ ] The change works at 390 px wide: nothing is cut off, overlaps, or makes the page scroll
      sideways (code, diffs and wide tables scroll inside their own box).
- [ ] New controls are reachable on a phone. Session header or footer additions also appear in the
      ⋯ sheet (`SessionSheet.tsx`); project-level actions appear in the drawer.
- [ ] Nothing needs hover, right-click, modifier-click, drag and drop, or a keyboard shortcut
      without a tap equivalent. Needed information isn't only in a `title` tooltip.
- [ ] Touch targets are at least 36 px tall (40 px for icon buttons) under `(pointer: coarse)`.
- [ ] New text inputs are 16 px or larger in the phone section of `styles.css`.
- [ ] Full-height views use `var(--app-h, 100dvh)` on phones, not `100vh`. Bars at the screen edges
      respect `env(safe-area-inset-*)`.
- [ ] Phone-only behavior uses `MOBILE_QUERY` / `TOUCH_QUERY` from `hooks/useMedia.ts` and the
      matching CSS media queries, not new ad-hoc breakpoints or user-agent sniffing.
- [ ] The desktop layout is unchanged unless the change is meant for it.
- [ ] Touch input to the terminal goes through `terminals.ts` / `keys.ts`, and `keys.ts` changes
      have tests. The terminal doesn't take focus on touch screens outside typing mode.
- [ ] `docs/screenshots/mobile.png` is retaken if the phone session screen changed visibly.
