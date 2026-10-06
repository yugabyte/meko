# AGENTS.md: the web UI

Extra rules for changes under `src/web/`. The repo-wide guide is [../../AGENTS.md](../../AGENTS.md);
reviewers use [REVIEW.md](REVIEW.md) here as well as the root one.

## Every UI change works on a phone

People drive sessions from their phones, so a change isn't done until it works at 390 px wide
with a finger. Before you finish a UI change:

- **Check it at phone width.** Open it in device emulation (a 390×844 phone, with touch on) as
  well as on the desktop, in both light and dark themes. Nothing may scroll sideways except code,
  diffs and wide tables inside their own `overflow-x: auto` box.
- **Use the existing breakpoints.** The phone layout is `@media (max-width: 760px)`; in code,
  `useIsMobile()` from `hooks/useMedia.ts` (`MOBILE_QUERY`). Touch sizing is `(pointer: coarse)` /
  `useTouch()`, and hover-only affordances need a `(hover: none)` fallback. Don't add new
  breakpoints; if you really need one, add it to `hooks/useMedia.ts` so CSS and code agree.
- **Prefer CSS over separate markup.** Restyle in the phone section at the end of `styles.css`.
  Render different markup only where the structure really differs (the app bar, the ⋯ sheet, the
  terminal key bar), and gate it on `useIsMobile()` / `useTouch()`.
- **Where do new controls go on a phone?** A new header or footer button on the session screen
  also needs a row in the ⋯ sheet (`SessionSheet.tsx`), because the phone layout doesn't render the
  crumb header or the stage bar (the sheet shows the stage bar instead) and shortens the footer. A new project-level action belongs in the drawer (`Rail.tsx`).
- **Touch targets** are at least 36 px tall (40 px for icon buttons). Don't rely on hover,
  right-click, Shift/⌘-click, drag and drop, or keyboard shortcuts alone; give each one a tap
  equivalent. Tooltips (`title`) aren't seen on touch, so don't hide needed information in them.
- **Text inputs are 16 px or larger on phones**, or iOS zooms in on focus. The phone section
  already covers `.input`, `textarea` and `select`; add any new input class there.
- **Heights follow the keyboard.** Size full-height views with `var(--app-h, 100dvh)` in the phone
  section, never a bare `100vh`, so the on-screen keyboard shrinks the view instead of covering it.
  A fixed layer over the whole screen takes `top: var(--app-top, 0px)` and `height: var(--app-h, 100dvh)`
  there, not `inset: 0`: iOS doesn't shrink the layout viewport, so its bottom is behind the keyboard.
  Things pinned to the top or bottom edge pad themselves with `env(safe-area-inset-*)`.
- **Dialogs** use `Dialog` from `common.tsx`, which is already a bottom sheet on phones. New
  bottom menus use `Sheet` from `SessionSheet.tsx`.
- **The terminal** takes touch input through `terminals.ts` (`sendKey`, `submit`, swipe scrolling)
  and the pure helpers in `keys.ts`, which have unit tests. Don't focus the terminal on touch
  screens unless typing mode is on (`term.typing`), or the keyboard pops up on every tap.

## In-progress labels

A label for something still running ("Starting", "Opening", "Loading") is `<Busy label="Starting" />`
from `common.tsx`, never a static "Starting…", which looks stuck. Its dots cycle `.` `..` `...`
by opacity (Safari doesn't animate `content`) and stay still with reduced motion.
`test/unit/busy-labels.test.ts` flags a static label like "Saving…" in a component.

## Reloads after an update

An open page reloads itself when the server comes back on another version (`reload.ts`), unless
the user is busy: a `role="dialog"` element is open, a text field has focus, a `textarea` holds
text, or a session's terminal holds images for Claude (`hasHeldImages()`, checked in
`useReload.ts`, since terminals outlast their pane). A field whose text survives a reload (kept in `sessionStorage`, like the terminal compose box)
gets `data-reload-safe` so it doesn't hold the reload up. Text kept anywhere else in a field that
isn't a focused input or a textarea is lost on the reload, so keep drafts that matter in a
textarea, a dialog, or storage.

## Checking it

`npm run dev` serves the UI on :5173. Use your browser's device toolbar with touch emulation. For a
real phone, open the installed instance's HTTPS address.
