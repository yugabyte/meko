// Per-file-type icons for the Files table (see `fileTypeVisual` in
// KnowledgeFilesTable.tsx).
//
// HTML/JSON use the "badge" artwork from the `vscode-icons` project
// (https://github.com/vscode-icons/vscode-icons), downloaded individually
// rather than pulled in as an npm dependency. Source code there is MIT;
// the icon artwork itself is Creative Commons Attribution-ShareAlike 4.0
// (https://creativecommons.org/licenses/by-sa/4.0/). These are generic
// file-type pictograms (not a vendor's trademarked logo) -- the HTML
// "shield", for instance, mirrors the shape W3C itself publishes under
// CC BY for exactly this purpose (https://www.w3.org/html/logo/).
//
// Sources (icons/file_type_*.svg on vscode-icons/vscode-icons @ master):
//   file_type_html.svg, file_type_json.svg
//
// PDF/TXT/CSV/Markdown instead use Tabler Icons (https://tabler.io/icons,
// MIT License -- see the LICENSE text below): PDF specifically used to be
// the vscode-icons "badge" too, but that artwork reproduces Adobe
// Acrobat's red-trapezoid mark closely enough to read as the trademarked
// logo rather than a generic pictogram, so it's replaced with Tabler's
// plain outline glyph. TXT/CSV/Markdown never used the badge style to
// begin with -- it read as too flat/plain at this size -- so all four
// stay as generic outline pictograms in the same 24x24 stroke-based
// style as the `lucide-react` icons used everywhere else in the app.
//
// Tabler Icons LICENSE (MIT):
//   Copyright (c) 2020-2026 Paweł Kuna
//   Permission is hereby granted, free of charge, to any person obtaining
//   a copy of this software and associated documentation files (the
//   "Software"), to deal in the Software without restriction, including
//   without limitation the rights to use, copy, modify, merge, publish,
//   distribute, sublicense, and/or sell copies of the Software, subject
//   to including this notice in all copies or substantial portions.
//   Full text: https://github.com/tabler/tabler-icons/blob/main/LICENSE
//
// Sources (icons/outline/*.svg on tabler/tabler-icons @ main):
//   file-type-pdf.svg, file-type-txt.svg, file-type-csv.svg, markdown.svg

import type { SVGProps } from "react";

type IconProps = SVGProps<SVGSVGElement>;

/** Shared wrapper for the vscode-icons badge artwork: fixed 32x32
 *  viewBox, every shape's color baked into its own `fill`, so (unlike
 *  the lucide-style outline icons elsewhere) `text-*` / `currentColor`
 *  utilities don't apply here -- the artwork already carries its own
 *  per-type color. */
function BadgeIcon({ children, ...props }: IconProps & { children: React.ReactNode }) {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" width="32" height="32" viewBox="0 0 32 32" {...props}>
      {children}
    </svg>
  );
}

/** Shared wrapper matching lucide-react's rendering (24x24 viewBox,
 *  stroke="currentColor" so `text-*` color utility classes apply,
 *  round caps/joins) so these icons are visually indistinguishable in
 *  weight/style from the rest of the app's iconography. */
function TablerIcon({ children, ...props }: IconProps & { children: React.ReactNode }) {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      width="24"
      height="24"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      {...props}
    >
      {children}
    </svg>
  );
}

export function FileTypePdfIcon(props: IconProps) {
  return (
    <TablerIcon {...props}>
      <path d="M14 3v4a1 1 0 0 0 1 1h4" />
      <path d="M5 12v-7a2 2 0 0 1 2 -2h7l5 5v4" />
      <path d="M5 18h1.5a1.5 1.5 0 0 0 0 -3h-1.5v6" />
      <path d="M17 18h2" />
      <path d="M20 15h-3v6" />
      <path d="M11 15v6h1a2 2 0 0 0 2 -2v-2a2 2 0 0 0 -2 -2h-1" />
    </TablerIcon>
  );
}

export function FileTypeHtmlIcon(props: IconProps) {
  return (
    <BadgeIcon {...props}>
      <polygon points="5.902 27.201 3.655 2 28.345 2 26.095 27.197 15.985 30 5.902 27.201" style={{ fill: "#e44f26" }} />
      <polygon points="16 27.858 24.17 25.593 26.092 4.061 16 4.061 16 27.858" style={{ fill: "#f1662a" }} />
      <polygon
        points="16 13.407 11.91 13.407 11.628 10.242 16 10.242 16 7.151 15.989 7.151 8.25 7.151 8.324 7.981 9.083 16.498 16 16.498 16 13.407"
        style={{ fill: "#ebebeb" }}
      />
      <polygon
        points="16 21.434 15.986 21.438 12.544 20.509 12.324 18.044 10.651 18.044 9.221 18.044 9.654 22.896 15.986 24.654 16 24.65 16 21.434"
        style={{ fill: "#ebebeb" }}
      />
      <polygon
        points="15.989 13.407 15.989 16.498 19.795 16.498 19.437 20.507 15.989 21.437 15.989 24.653 22.326 22.896 22.372 22.374 23.098 14.237 23.174 13.407 22.341 13.407 15.989 13.407"
        style={{ fill: "#fff" }}
      />
      <polygon
        points="15.989 7.151 15.989 9.071 15.989 10.235 15.989 10.242 23.445 10.242 23.445 10.242 23.455 10.242 23.517 9.548 23.658 7.981 23.732 7.151 15.989 7.151"
        style={{ fill: "#fff" }}
      />
    </BadgeIcon>
  );
}

export function FileTypeJsonIcon({ className, ...props }: IconProps) {
  // Upstream's fill is a flat #f5de19 -- reads great on VS Code's dark
  // sidebar (what it was designed for) but all but disappears against
  // a white/light background. `fill-*` gets a darker amber in light
  // mode and switches to the original bright yellow in dark mode,
  // rather than hardcoding one fill for both.
  const braceClass = "fill-amber-600 dark:fill-yellow-300";
  return (
    <BadgeIcon className={className} {...props}>
      <path
        className={braceClass}
        d="M4.014,14.976a2.51,2.51,0,0,0,1.567-.518A2.377,2.377,0,0,0,6.386,13.1,15.261,15.261,0,0,0,6.6,10.156q.012-2.085.075-2.747a5.236,5.236,0,0,1,.418-1.686,3.025,3.025,0,0,1,.755-1.018A3.046,3.046,0,0,1,9,4.125,6.762,6.762,0,0,1,10.544,4h.7V5.96h-.387a2.338,2.338,0,0,0-1.723.468A3.4,3.4,0,0,0,8.709,8.52a36.054,36.054,0,0,1-.137,4.133,4.734,4.734,0,0,1-.768,2.06A4.567,4.567,0,0,1,6.1,16a3.809,3.809,0,0,1,1.992,1.754,8.861,8.861,0,0,1,.618,3.865q0,2.435.05,2.9A1.755,1.755,0,0,0,9.264,25.7a2.639,2.639,0,0,0,1.592.337h.387V28h-.7a5.655,5.655,0,0,1-1.773-.2,2.97,2.97,0,0,1-1.324-.93,3.353,3.353,0,0,1-.681-1.63A24.175,24.175,0,0,1,6.6,22.006,16.469,16.469,0,0,0,6.386,18.9a2.408,2.408,0,0,0-.805-1.361,2.489,2.489,0,0,0-1.567-.524Z"
      />
      <path
        className={braceClass}
        d="M27.986,17.011a2.489,2.489,0,0,0-1.567.524,2.408,2.408,0,0,0-.805,1.361,16.469,16.469,0,0,0-.212,3.109,24.175,24.175,0,0,1-.169,3.234,3.353,3.353,0,0,1-.681,1.63,2.97,2.97,0,0,1-1.324.93,5.655,5.655,0,0,1-1.773.2h-.7V26.04h.387a2.639,2.639,0,0,0,1.592-.337,1.755,1.755,0,0,0,.506-1.186q.05-.462.05-2.9a8.861,8.861,0,0,1,.618-3.865A3.809,3.809,0,0,1,25.9,16a4.567,4.567,0,0,1-1.7-1.286,4.734,4.734,0,0,1-.768-2.06,36.054,36.054,0,0,1-.137-4.133,3.4,3.4,0,0,0-.425-2.092,2.338,2.338,0,0,0-1.723-.468h-.387V4h.7A6.762,6.762,0,0,1,23,4.125a3.046,3.046,0,0,1,1.149.581,3.025,3.025,0,0,1,.755,1.018,5.236,5.236,0,0,1,.418,1.686q.062.662.075,2.747a15.261,15.261,0,0,0,.212,2.947,2.377,2.377,0,0,0,.805,1.355,2.51,2.51,0,0,0,1.567.518Z"
      />
    </BadgeIcon>
  );
}

export function FileTypeMarkdownIcon(props: IconProps) {
  return (
    <TablerIcon {...props}>
      <path d="M3 7a2 2 0 0 1 2 -2h14a2 2 0 0 1 2 2v10a2 2 0 0 1 -2 2h-14a2 2 0 0 1 -2 -2v-10" />
      <path d="M7 15v-6l2 2l2 -2v6" />
      <path d="M14 13l2 2l2 -2m-2 2v-6" />
    </TablerIcon>
  );
}

export function FileTypeTxtIcon(props: IconProps) {
  return (
    <TablerIcon {...props}>
      <path d="M14 3v4a1 1 0 0 0 1 1h4" />
      <path d="M16.5 15h3" />
      <path d="M5 12v-7a2 2 0 0 1 2 -2h7l5 5v4" />
      <path d="M4.5 15h3" />
      <path d="M6 15v6" />
      <path d="M18 15v6" />
      <path d="M10 15l4 6" />
      <path d="M10 21l4 -6" />
    </TablerIcon>
  );
}

export function FileTypeCsvIcon(props: IconProps) {
  return (
    <TablerIcon {...props}>
      <path d="M14 3v4a1 1 0 0 0 1 1h4" />
      <path d="M5 12v-7a2 2 0 0 1 2 -2h7l5 5v4" />
      <path d="M7 16.5a1.5 1.5 0 0 0 -3 0v3a1.5 1.5 0 0 0 3 0" />
      <path d="M10 20.25c0 .414 .336 .75 .75 .75h1.25a1 1 0 0 0 1 -1v-1a1 1 0 0 0 -1 -1h-1a1 1 0 0 1 -1 -1v-1a1 1 0 0 1 1 -1h1.25a.75 .75 0 0 1 .75 .75" />
      <path d="M16 15l2 6l2 -6" />
    </TablerIcon>
  );
}
