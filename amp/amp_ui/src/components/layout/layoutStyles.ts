// Single source of truth for app-shell styling (sidebar + header + main).
//
// Every layout and page-level component imports class strings from here
// instead of hard-coding Tailwind on the spot, so the shell's look lives
// in ONE file and can't drift between screens. Compose with `cn()` from
// `@/lib/utils` when a call site needs a one-off tweak.
//
// Colours resolve through the semantic + brand tokens defined in
// `src/index.css` (`bg-sidebar`, `bg-brand`, `border-border`, …), so
// everything here re-themes automatically on light/dark switch.

export const layoutStyles = {
  /* ---- Shell ---- */
  shell: "flex h-screen w-full overflow-hidden bg-background text-foreground",
  content: "flex min-w-0 flex-1 flex-col",
  main: "flex-1 overflow-y-auto",

  /* ---- Sidebar ---- */
  sidebar:
    "flex h-full w-72 shrink-0 flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground",
  brand: "flex items-center gap-3 px-5 py-5",
  // Orange app-icon tile with a soft glow; the dark aperture dot is
  // rendered as a child (see `brandDot`).
  brandMark:
    "flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-gradient-to-br from-orange-400 to-brand shadow-lg shadow-brand/30",
  brandDot: "h-3.5 w-3.5 rounded-full bg-[#0A0F1C]",
  brandName: "text-base font-bold leading-tight text-sidebar-foreground",
  // Tight tracking + nowrap so "AGENTIC POSTGRES PLATFORM" stays on one
  // line within the (slightly wider) rail.
  brandTag:
    "whitespace-nowrap text-[10px] font-medium uppercase tracking-[0.03em] text-muted-foreground",

  nav: "flex-1 overflow-y-auto px-3 pb-4",
  sectionLabel:
    "px-3 pb-1 pt-5 text-[11px] font-semibold uppercase tracking-[0.12em] text-muted-foreground",

  // Nav item — base is always applied; `navItemActive` / `navItemInactive`
  // are swapped by NavLink's isActive callback.
  navItem:
    "group relative flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
  navItemActive: "bg-sidebar-accent text-sidebar-accent-foreground",
  navItemInactive:
    "text-muted-foreground hover:bg-sidebar-accent/60 hover:text-sidebar-foreground",
  navIcon: "h-[18px] w-[18px] shrink-0",
  navLabel: "truncate",
  navBadge:
    "ml-auto rounded-full bg-sidebar-accent px-2 py-0.5 text-[11px] font-medium text-muted-foreground",
  statusDot: "ml-auto h-2 w-2 rounded-full bg-emerald-400 shadow-[0_0_6px] shadow-emerald-400/60",

  /* ---- Platform-model legend ---- */
  platformBox: "mx-2 mt-5 rounded-xl border border-dashed border-sidebar-border p-3.5",
  platformTitle: "text-sm font-semibold text-sidebar-foreground",
  platformList: "mt-3 flex flex-col gap-2",
  platformRow: "flex items-center gap-2 whitespace-nowrap text-xs",
  platformSwatch: "h-3 w-3 shrink-0 rounded-sm",
  platformLabel: "font-medium text-sidebar-foreground",
  platformMeta: "text-muted-foreground",

  /* ---- Sidebar footer (logged-in user) ---- */
  footer: "border-t border-sidebar-border p-3",
  userRow:
    "flex w-full items-center gap-3 rounded-md px-2 py-2 text-left transition-colors hover:bg-sidebar-accent/60",
  userAvatar:
    "flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-gradient-to-br from-indigo-400 to-purple-500 text-sm font-semibold text-white",
  userName: "truncate text-sm font-medium text-sidebar-foreground",
  userMeta: "truncate text-xs text-muted-foreground",

  /* ---- Header ---- */
  header:
    "flex h-16 shrink-0 items-center gap-4 border-b border-border bg-background px-4 sm:px-6",
  searchWrap: "relative mx-auto hidden w-full max-w-xl md:block",
  searchIcon:
    "pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-brand",
  // `border-border` + `bg-card` override the Input default (`border-input`
  // + transparent), which is invisible against the header in dark mode.
  // This gives the field a clear box in both light and dark.
  searchInput: "h-10 w-full rounded-lg border-border bg-card pl-9 pr-12",
  searchKbd:
    "pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 rounded border border-border bg-muted px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground",
  headerActions: "ml-auto flex items-center gap-1",

  /* ---- Agent side panel ---- */
  panelContent: "flex w-full flex-col gap-6 sm:max-w-md",
  panelHeader: "flex items-center gap-3 border-b border-border pb-4 pr-8",
  panelTile: "flex h-11 w-11 shrink-0 items-center justify-center rounded-xl",
  panelTileIcon: "h-5 w-5",
  panelTitle: "text-lg font-semibold leading-tight text-foreground",
  panelSubtitle: "text-sm text-muted-foreground",
  panelBodyRow: "flex gap-3",
  panelTileSm: "flex h-9 w-9 shrink-0 items-center justify-center rounded-lg",
  panelTileSmIcon: "h-[18px] w-[18px]",
  panelLabel: "text-xs font-semibold uppercase tracking-[0.12em]",
  panelText: "mt-1 text-sm leading-relaxed text-foreground",
  panelCta:
    "h-12 w-full bg-brand text-base font-semibold text-brand-foreground shadow-lg shadow-brand/30 hover:bg-brand/90",
} as const;
