// The two second-row cards — what the knowledge base holds, and what Meko has
// captured out of conversations. Both are a stat pair plus one derived line,
// so they share a file rather than each getting a near-empty one.

import { formatFileSize } from "@/lib/fileSize";
import { CONTEXT_TONE } from "@/lib/contextTones";

import { MetricCard, MetricStat, MetricStatDivider, MetricStatRow } from "./MetricCard";

/** Loading skeleton for a stat pair, shared by both cards below. */
function StatPairSkeleton() {
  return (
    <div className="flex gap-6">
      {[0, 1].map((i) => (
        <div key={i} className="space-y-1">
          <div className="h-8 w-12 animate-pulse rounded bg-muted" />
          <div className="h-3 w-16 animate-pulse rounded bg-muted" />
        </div>
      ))}
    </div>
  );
}

export function KnowledgeBaseCard({
  loading,
  chunkCount,
  fileCount,
  bytes,
}: {
  loading: boolean;
  chunkCount: number | null;
  fileCount: number | null;
  bytes: number | null;
}) {
  return (
    <MetricCard title="Knowledge Base" subtitle="Indexed chunks, files and storage">
      {loading ? (
        <StatPairSkeleton />
      ) : (
        <>
          <MetricStatRow>
            <MetricStat value={chunkCount?.toLocaleString() ?? "—"} label="Vector chunks" />
            <MetricStatDivider />
            <MetricStat value={fileCount?.toLocaleString() ?? "—"} label="Files" />
          </MetricStatRow>
          <p className="text-xs text-muted-foreground">
            Total size:{" "}
            <span className="font-medium text-foreground">
              {bytes && bytes > 0 ? formatFileSize(bytes) : "—"}
            </span>
          </p>
        </>
      )}
    </MetricCard>
  );
}

/** Five memories per conversation is a full bar — beyond that the rate stops
 *  being informative and the bar would just read as "max". */
const EXTRACTION_RATE_FULL = 5;

export function CapturedExtractedCard({
  loading,
  conversationCount,
  extractedCount,
  extractionRate,
}: {
  loading: boolean;
  conversationCount: number | null;
  extractedCount: number | null;
  extractionRate: number | null;
}) {
  return (
    <MetricCard title="Captured & Extracted" subtitle="Conversations indexed into this datapack">
      {loading ? (
        <StatPairSkeleton />
      ) : (
        <>
          <MetricStatRow>
            <MetricStat value={conversationCount?.toLocaleString() ?? "—"} label="Conversations" />
            <MetricStatDivider />
            <MetricStat
              value={extractedCount?.toLocaleString() ?? "—"}
              label="Memories extracted"
            />
          </MetricStatRow>

          {conversationCount != null && conversationCount > 0 && extractionRate != null && (
            <div className="space-y-1.5">
              <p className="text-xs font-medium text-muted-foreground">Extraction rate</p>
              <div className="h-1 w-full overflow-hidden rounded-full bg-muted">
                <div
                  // Extraction turns a conversation into memories, so the bar
                  // carries memory's hue rather than a colour of its own.
                  className={`h-full rounded-full ${CONTEXT_TONE.memory.fill}`}
                  style={{
                    width: `${Math.min(100, (extractionRate / EXTRACTION_RATE_FULL) * 100)}%`,
                  }}
                />
              </div>
              <p className="text-xs text-muted-foreground">
                {extractionRate.toFixed(2)} memories / conversation
              </p>
            </div>
          )}
        </>
      )}
    </MetricCard>
  );
}
