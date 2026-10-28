// The three context tiles at the top of Overview, as Meko Cloud's datapack page shows them: how big
// the datapack's context is, how much of it a search saves sending, and how relevant its results are.

import type { ReactNode } from "react";
import { Database, Sparkles, Zap } from "lucide-react";
import { count } from "../../lib/format";
import { formatTokens, type Derived } from "../../lib/metrics";
import { Badge, Card, cx } from "../ui";

function Tile({
  icon,
  tone,
  label,
  badge,
  value,
  hint,
}: {
  icon: ReactNode;
  tone: string;
  label: string;
  badge?: ReactNode;
  value: string;
  hint: string;
}) {
  return (
    <Card className="p-4">
      <div className="flex items-center gap-2.5">
        <span className={cx("flex size-8 items-center justify-center rounded-lg [&>svg]:size-4", tone)}>{icon}</span>
        <span className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{label}</span>
        {badge}
      </div>
      <div className="mt-3 text-2xl font-semibold tabular-nums">{value}</div>
      <div className="mt-0.5 text-xs text-muted-foreground">{hint}</div>
    </Card>
  );
}

export function ContextTiles({ d }: { d: Derived }) {
  const quality = d.qualityScore;
  return (
    <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
      <Tile
        icon={<Database />}
        tone="bg-info/15 text-info"
        label="Context size"
        value={`${formatTokens(d.tokens.total)} tokens`}
        hint="KB · memory · conversations"
      />
      <Tile
        icon={<Zap />}
        tone="bg-success/15 text-success"
        label="Context efficiency"
        badge={d.savedPct !== null && <Badge tone="success">live</Badge>}
        value={d.savedPct !== null ? `${Math.round(d.savedPct)}% saved` : "No searches yet"}
        hint={
          d.avgTokensSent !== null && d.avgTokensIfRaw !== null
            ? `~${formatTokens(d.avgTokensSent)} sent vs ~${formatTokens(d.avgTokensIfRaw)} baseline / query`
            : "Fills in once agents search this datapack"
        }
      />
      <Tile
        icon={<Sparkles />}
        tone="bg-warning/15 text-warning"
        label="Context quality"
        value={quality !== null ? `${Math.round(100 * quality)}% relevant` : "No results yet"}
        hint={quality !== null ? `across ${count(d.scores.total)} results` : "Share of results scoring 0.50 or more"}
      />
    </div>
  );
}
