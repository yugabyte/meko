// Who the reviewing agent is: name, licence, liveness, and the facts that
// matter before you hand it your datapack — where it runs, how it reads
// context, and how much context it can see right now.
//
// Every row comes from the resolved deployment or the lab definition. A fact
// that isn't knowable (no region in the ARN, no deployment yet) is omitted
// rather than guessed.

import type { ReactNode } from "react";

import { Badge } from "@/components/ui/badge";
import type { Deployment } from "@/lib/api/deploymentsApi";

import { regionFromRuntimeArn } from "../../lib/agentRuntimeArn";

function Row({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4 border-t border-border py-2 first:border-t-0">
      <dt className="shrink-0 text-sm text-muted-foreground">{label}</dt>
      <dd className="min-w-0 truncate font-mono text-xs text-foreground">{value}</dd>
    </div>
  );
}

export function AgentIdentityCard({
  agentDisplayName,
  agentSlug,
  sourceRepoUrl,
  deployment,
  /** True once the datapack has been shared with the agent — the whole point
   *  of the next lesson, so it reads "none" until then. */
  hasContext,
}: {
  agentDisplayName: string;
  agentSlug: string;
  sourceRepoUrl: string;
  deployment: Deployment | null;
  hasContext: boolean;
}) {
  const region = regionFromRuntimeArn(deployment?.agent_runtime_arn);
  const isActive = deployment?.status === "active";

  return (
    <section className="flex gap-4 rounded-lg border border-border bg-card p-4">
      <span
        aria-hidden
        className="flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-muted text-sm font-semibold text-foreground"
      >
        {agentDisplayName.slice(0, 2).toUpperCase()}
      </span>

      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="text-sm font-semibold text-foreground">{agentDisplayName}</h3>
          {deployment && (
            <Badge variant="secondary" className="text-[10px]">
              {isActive ? "online" : deployment.status}
            </Badge>
          )}
          <Badge variant="secondary" className="text-[10px]">
            Apache-2.0
          </Badge>
        </div>

        <p className="mt-1.5 text-sm text-muted-foreground">
          Reviews a diff against whatever context it can read, cites the rule behind every comment, and
          stops at the guardrail you set.
        </p>

        <dl className="mt-3">
          <Row label="Runtime" value={region ? `AWS Bedrock AgentCore · ${region}` : "AWS Bedrock AgentCore"} />
          <Row label="Agent ID" value={agentSlug} />
          <Row label="Reads context via" value="MCP" />
          <Row label="Context today" value={hasContext ? "1 datapack" : "none, 0 datapacks"} />
          <Row
            label="Source"
            value={
              <a
                href={sourceRepoUrl}
                target="_blank"
                rel="noreferrer"
                className="text-primary underline"
              >
                view the source
              </a>
            }
          />
        </dl>
      </div>
    </section>
  );
}
