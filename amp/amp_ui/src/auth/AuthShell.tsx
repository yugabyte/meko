// Full-viewport backdrop shared by every unauthenticated AMP page
// (Sign in, Sign up). Adapted from meko_ui's AuthPageShell: an ambient
// aurora gradient with separate light/dark layers (toggled by the
// `.dark` class on <html>), an AMP wordmark top-left, a theme toggle
// top-right, and a small legal footer.

import type { CSSProperties, ReactNode } from "react";
import { ThemeToggle } from "@/components/ThemeToggle";

// Saturated purple / emerald / blue glows over near-black for dark mode.
const AURORA_DARK_BACKGROUND: CSSProperties["background"] =
  "radial-gradient(ellipse 80% 50% at 18% 22%, rgba(139, 92, 246, 0.28), transparent 60%)," +
  "radial-gradient(ellipse 70% 50% at 82% 68%, rgba(16, 185, 129, 0.22), transparent 60%)," +
  "radial-gradient(ellipse 60% 70% at 50% 110%, rgba(59, 130, 246, 0.20), transparent 55%)," +
  "linear-gradient(180deg, #0A0F1C 0%, #0E1525 50%, #0A0F1C 100%)";

// Same geometry, softened opacities so the glows don't wash out the
// near-white light-mode base.
const AURORA_LIGHT_BACKGROUND: CSSProperties["background"] =
  "radial-gradient(ellipse 80% 50% at 18% 22%, rgba(139, 92, 246, 0.16), transparent 60%)," +
  "radial-gradient(ellipse 70% 50% at 82% 68%, rgba(16, 185, 129, 0.14), transparent 60%)," +
  "radial-gradient(ellipse 60% 70% at 50% 110%, rgba(59, 130, 246, 0.12), transparent 55%)," +
  "linear-gradient(180deg, #FCFDFF 0%, #EEF2F8 50%, #FCFDFF 100%)";

export function AuthShell({ children }: { children: ReactNode }) {
  return (
    <div className="relative flex min-h-screen flex-col text-foreground">
      {/* Two backdrop layers; CSS hides the wrong one via the `.dark`
          ancestor class. `-z-10` keeps them behind all content. */}
      <div
        aria-hidden
        className="absolute inset-0 -z-10 block dark:hidden"
        style={{ background: AURORA_LIGHT_BACKGROUND }}
      />
      <div
        aria-hidden
        className="absolute inset-0 -z-10 hidden dark:block"
        style={{ background: AURORA_DARK_BACKGROUND }}
      />

      <header className="relative z-10 flex items-center justify-between px-6 py-5 sm:px-8">
        <span className="text-2xl font-bold tracking-tight sm:text-3xl">
          AMP
        </span>
        <ThemeToggle />
      </header>

      <main className="relative z-10 flex flex-1 items-center justify-center px-4 py-4">
        {children}
      </main>

      <footer className="relative z-10 px-6 pb-5 pt-3 text-center text-xs text-muted-foreground sm:px-8">
        © {new Date().getFullYear()} Yugabyte, Inc. All rights reserved.
      </footer>
    </div>
  );
}
