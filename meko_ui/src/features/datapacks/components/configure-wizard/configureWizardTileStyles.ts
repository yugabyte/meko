import { cn } from "@/lib/utils";

export type WizardSyncTone = "emerald" | "blue" | "purple";

export const wizardTileToneBorder: Record<WizardSyncTone, string> = {
  emerald: "border-emerald-500/35 bg-emerald-950/20",
  blue: "border-blue-500/35 bg-blue-950/20",
  purple: "border-purple-500/35 bg-purple-950/20",
};

export const wizardTileToneBadge: Record<WizardSyncTone, string> = {
  emerald: "border-emerald-500/40 bg-emerald-500/15 text-emerald-100",
  blue: "border-blue-500/40 bg-blue-500/15 text-blue-100",
  purple: "border-purple-500/40 bg-purple-500/15 text-purple-100",
};

export const wizardTileToneIcon: Record<WizardSyncTone, string> = {
  emerald: "text-emerald-400",
  blue: "text-blue-400",
  purple: "text-purple-400",
};

export const wizardTileToneLink: Record<WizardSyncTone, string> = {
  emerald: "text-emerald-400 hover:text-emerald-300",
  blue: "text-blue-400 hover:text-blue-300",
  purple: "text-purple-400 hover:text-purple-300",
};

export function wizardTileRootClass(tone: WizardSyncTone, extra?: string) {
  return cn(
    "flex min-h-[280px] min-w-0 flex-1 flex-col rounded-xl border-2 p-4 sm:p-5",
    wizardTileToneBorder[tone],
    extra,
  );
}
