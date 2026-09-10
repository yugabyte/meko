// Single-hue chart colors for the executive dashboard, kept deliberately spare
// so the charts stay easy to read: the Meko brand orange for datapacks + token
// savings, and a stock Tailwind emerald for KB documents (index.css keeps the
// emerald accent on Tailwind's palette rather than minting a token for it).
//
// CHART_ORANGE reads the `--brand` CSS variable (as `bg-brand`/SavingsHero do),
// so it is the real brand orange and tracks any theme change; recharts resolves
// the var in the SVG fill just like the tooltip's hsl(var(--card)) below.
export const CHART_ORANGE = "hsl(var(--brand))";
export const CHART_GREEN = "hsl(160 84% 39%)";

// Cycled per-bar palette shared by the dashboard bar charts, echoing the metric
// tile accents (blue/orange/violet/emerald/pink/cyan/amber/sky).
export const BAR_COLORS = [
  "hsl(217 91% 60%)",
  "hsl(25 95% 53%)",
  "hsl(258 90% 66%)",
  "hsl(160 84% 39%)",
  "hsl(330 81% 60%)",
  "hsl(189 94% 43%)",
  "hsl(43 96% 56%)",
  "hsl(199 89% 48%)",
];
