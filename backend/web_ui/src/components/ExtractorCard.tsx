// The Status page's memory extractor: off (with the command that turns it on), or the AI tool it
// runs, how often, what waits, and how its last run went.

import { api } from "../lib/api";
import { dateTime } from "../lib/format";
import { useLoad } from "../lib/useLoad";
import { CopyBlock } from "./CopyBlock";
import { EXTRACTOR_COMMAND } from "./ExtractionBanner";
import { KeyValues } from "./KeyValues";
import { MetricCard } from "./metrics/MetricCard";
import { Badge } from "./ui";

export function ExtractorCard() {
  const { data: x } = useLoad(() => api.memoryExtractor(), []);
  if (!x) return null;
  return (
    <MetricCard title="Memory extraction">
      {!x.enabled ? (
        <div className="space-y-2 text-sm">
          <p>
            <Badge tone="warning">off</Badge> Memories aren't extracted from conversations. Turn it on with:
          </p>
          <CopyBlock text={EXTRACTOR_COMMAND} />
        </div>
      ) : (
        <KeyValues
          rows={[
            ["State", <Badge tone={x.last_error ? "danger" : "success"}>{x.last_error ? "failing" : "on"}</Badge>],
            ["Through", `${x.harness} (${x.model}), every ${Math.round(x.interval_seconds / 60)} min`],
            ["Waiting", `${x.queued} turns`],
            ["Extracted", `${x.extracted} memories`],
            ["Last run", x.last_run_at ? dateTime(x.last_run_at) : "not yet"],
            ...(x.last_error ? [["Last error", x.last_error] as [string, string]] : []),
          ]}
        />
      )}
    </MetricCard>
  );
}
