// Guards the one rule that keeps the five context concepts on one palette:
// a migrated file may not name a colour. Knowledge / memory / conversation /
// learning / collective resolve through lib/contextTones (and therefore the
// `--context-*` tokens in index.css), and status through
// success / warning / destructive / info — so retuning any of them is one
// edit in one file.
//
// Lives next to contextTones.ts rather than under charts/ because it stopped
// being about charts: it covers the metrics dashboard, the detail page's
// pipeline, the knowledge-base surfaces and the memory panels.
//
// It exists because the colours HAD drifted. Each of those surfaces
// hard-coded its own emerald/blue/purple, and they only ever agreed when
// someone matched them by hand. A grep in a test is what stops the next
// copy-paste.

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

const ROOTS = [
  "src/components/ui/charts",
  "src/features/datapacks/components/datapack-detail/metrics",
];

/** Individual files that have been migrated onto the tokens. Listed one by
 *  one rather than by folder because their neighbours haven't been done yet —
 *  adding a file here is how a migration gets locked in. */
const FILES = [
  "src/features/datapacks/components/datapack-detail/CollectiveLearningPipeline.tsx",
  "src/features/datapacks/components/datapack-detail/DatapackMetricsSection.tsx",
  "src/features/datapacks/components/datapack-detail/DatapackMetricsTilesRow.tsx",
  "src/features/datapacks/components/datapack-detail/DatapackOnboardingStepper.tsx",
  "src/features/datapacks/components/knowledge-documents/KnowledgeSummaryHeader.tsx",
  "src/features/datapacks/components/collective-memory/PromotedMemoryCard.tsx",
  "src/features/datapacks/components/conversations-viewer/MemoryHoverCard.tsx",
];

/** Tailwind palette families that carry meaning somewhere in this app. A
 *  chart naming one of these is defining a concept's colour locally. */
const PALETTE =
  /\b(?:slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-(?:50|[1-9]00|950)\b/;
/** A literal hex, in a class, a style object, or an SVG attribute. */
const HEX = /#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})\b/;

/** index.css sanctions a few literals — brand-art gradient endpoints, and
 *  visuals that must not move with the theme. A line preceded by this marker
 *  is allowed, which keeps the escape hatch visible and argued rather than
 *  turning it into a hole in the rule. */
const EXEMPT = "token-exempt:";

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((entry) => {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) return walk(full);
    return /\.tsx?$/.test(full) && !full.endsWith(".test.ts") && !full.endsWith(".test.tsx")
      ? [full]
      : [];
  });
}

/** Comments explain the history (which palette stop a token replaced), so
 *  they are not violations — only code is. */
function codeLines(source: string): { line: string; n: number }[] {
  let inBlock = false;
  let exemptNext = false;
  return source.split("\n").flatMap((raw, i) => {
    const line = raw.trim();
    if (inBlock) {
      if (line.includes("*/")) inBlock = false;
      return [];
    }
    if (line.startsWith("/*")) {
      if (!line.includes("*/")) inBlock = true;
      return [];
    }
    if (line.startsWith("//") || line.startsWith("*")) {
      // The marker covers the next code line, so a justified literal carries
      // its justification immediately above it.
      if (line.includes(EXEMPT)) exemptNext = true;
      return [];
    }
    if (exemptNext) {
      exemptNext = false;
      return [];
    }
    return [{ line: raw, n: i + 1 }];
  });
}

describe("context and status colours are named only in index.css", () => {
  const files = [...ROOTS.flatMap(walk), ...FILES];

  it("finds the files it is meant to be guarding", () => {
    // A rename that empties ROOTS/FILES would otherwise make this suite pass
    // by checking nothing at all.
    expect(files.length).toBeGreaterThan(20);
  });

  it.each(files)("%s uses tokens, not palette classes or hex", (file) => {
    const offenders = codeLines(readFileSync(file, "utf8"))
      .filter(({ line }) => PALETTE.test(line) || HEX.test(line))
      .map(({ line, n }) => `${n}: ${line.trim()}`);

    expect(offenders).toEqual([]);
  });
});
