// Outer wrapper for every unauthenticated route (Sign in, Sign up,
// Forgot password, Complete-new-password, future verification pages).
// Centralises the full-viewport backdrop.
//
// Theme handling — both backgrounds are rendered as absolutely-positioned
// layers behind the children; Tailwind's `dark:hidden` / `hidden dark:block`
// toggles which one is visible based on the `.dark` class on `<html>`.

import type { CSSProperties, ReactNode } from "react";

/**
 * Visual variant for the auth backdrop. Default `"aurora"`.
 *
 * Every variant is a design-maintenance surface, so add cautiously —
 * future tweaks need to be re-tested across all listed values AND
 * across light + dark mode.
 */
export type AuthShellVariant = "aurora";

type Props = {
  children: ReactNode;
  variant?: AuthShellVariant;
};

// --- Dark aurora --------------------------------------------------- //
// Saturated purple / emerald / blue glows over near-black. The
// ellipses are wider than the viewport so colour transitions feel like
// ambient atmosphere rather than discrete spotlights.
const AURORA_DARK_BACKGROUND: CSSProperties["background"] =
  "radial-gradient(ellipse 80% 50% at 18% 22%, rgba(139, 92, 246, 0.28), transparent 60%)," +
  "radial-gradient(ellipse 70% 50% at 82% 68%, rgba(16, 185, 129, 0.22), transparent 60%)," +
  "radial-gradient(ellipse 60% 70% at 50% 110%, rgba(59, 130, 246, 0.20), transparent 55%)," +
  "linear-gradient(180deg, #0A0F1C 0%, #0E1525 50%, #0A0F1C 100%)";

// --- Light aurora -------------------------------------------------- //
// Same geometry as dark; glow opacities are roughly halved so they
// don't wash out the near-white base. Base matches the `--background`
// token in light mode (`#F6F8FC` family) so the gradient flows into
// the rest of the page chrome instead of standing out as a separate
// canvas.
const AURORA_LIGHT_BACKGROUND: CSSProperties["background"] =
  "radial-gradient(ellipse 80% 50% at 18% 22%, rgba(139, 92, 246, 0.16), transparent 60%)," +
  "radial-gradient(ellipse 70% 50% at 82% 68%, rgba(16, 185, 129, 0.14), transparent 60%)," +
  "radial-gradient(ellipse 60% 70% at 50% 110%, rgba(59, 130, 246, 0.12), transparent 55%)," +
  "linear-gradient(180deg, #FCFDFF 0%, #EEF2F8 50%, #FCFDFF 100%)";

const BACKGROUNDS_LIGHT: Record<AuthShellVariant, CSSProperties["background"]> = {
  aurora: AURORA_LIGHT_BACKGROUND,
};
const BACKGROUNDS_DARK: Record<AuthShellVariant, CSSProperties["background"]> = {
  aurora: AURORA_DARK_BACKGROUND,
};

// Legal links rendered in the footer, in display order. Kept as data so the
// separators aren't hand-repeated between each anchor.
const LEGAL_LINKS: ReadonlyArray<{ href: string; label: string }> = [
  { href: "https://www.yugabyte.com/privacy-policy/", label: "Privacy Policy" },
  { href: "https://www.yugabyte.com/terms-of-service/", label: "Terms of Service" },
];

/**
 * Wraps an auth page card with the project's full-viewport backdrop,
 * a Meko logo pinned top-left, and a copyright + privacy footer at the
 * bottom. Children should render the card / form unchanged; the shell
 * handles centering, min-height, padding, ambient background (light /
 * dark mode picked automatically via the `.dark` class on `<html>`),
 * and the page chrome.
 */
export function AuthPageShell({ children, variant = "aurora" }: Props) {
  return (
    <div className="relative flex min-h-screen flex-col text-foreground">
      {/*
        Two background layers — one for light, one for dark. Both
        mount; Tailwind hides the wrong one via the `.dark` ancestor
        class. `-z-10` keeps them behind every other layer (logo, card,
        footer); `aria-hidden` keeps screen readers focused on the auth
        form. Inline style because the multi-stop `background` shorthand
        doesn't fit Tailwind's arbitrary-property syntax (commas + parens).
      */}
      <div
        aria-hidden
        className="absolute inset-0 -z-10 block dark:hidden"
        style={{ background: BACKGROUNDS_LIGHT[variant] }}
      />
      <div
        aria-hidden
        className="absolute inset-0 -z-10 hidden dark:block"
        style={{ background: BACKGROUNDS_DARK[variant] }}
      />

      {/*
        Brand header — Meko logo top-left, mirrors the Sidebar's
        expanded-state logo. Both light + dark variants ship; CSS
        toggles which one is visible via the `.dark` ancestor class
        (same pattern as `Sidebar.tsx`). Logo is sized smaller than
        the sidebar version (h-8 vs h-12) so it doesn't compete with
        the auth card for visual weight.
      */}
      <header className="relative z-10 flex items-center px-6 py-5 sm:px-8">
        {/* `h-8` on mobile, `h-14` from `sm:` (640px) up so the brand
            has real presence on desktop while staying tight on phones
            where vertical space is precious. */}
        <img src="/Meko_Logo_Blk.svg" alt="Meko" className="h-8 w-auto sm:h-14 block dark:hidden" />
        <img src="/meko-logo.svg" alt="Meko" className="h-8 w-auto sm:h-14 hidden dark:block" />
      </header>

      {/* The card sits in a flex-1 container so it stays vertically
          centred between the logo header and the footer regardless of
          how tall the form gets. */}
      <main className="relative z-10 flex flex-1 items-center justify-center px-4 py-4">
        {children}
      </main>

      {/*
        Legal footer — kept tiny + muted so it doesn't compete with the sign-in flow.
      */}
      <footer className="relative z-10 px-6 pb-5 pt-3 text-center text-xs text-muted-foreground sm:px-8">
        © {new Date().getFullYear()} Yugabyte, Inc. All rights reserved.
        {LEGAL_LINKS.map((link) => (
          <span key={link.href}>
            {" · "}
            <a
              href={link.href}
              target="_blank"
              rel="noopener noreferrer"
              className="underline underline-offset-2 hover:text-foreground"
            >
              {link.label}
            </a>
          </span>
        ))}
      </footer>
    </div>
  );
}
