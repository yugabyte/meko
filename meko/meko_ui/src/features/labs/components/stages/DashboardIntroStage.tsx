// Pure explainer -- orients the user on why a dashboard needs real
// relational storage before the next two stages provision one (AMP) and use
// it (run another review, see the dashboard). No data dependency, so unlike
// most of this chapter's stages it isn't gated behind LabStageNeedsDatapack.

import { Database } from "lucide-react";

const WHY_NOT_MEMORY = [
  {
    title: "Memory is for facts, not rows",
    body: "Meko's memory store holds discrete facts and preferences -- great for \"remember this,\" not for \"how many reviews ran this week\" or \"average cost per model.\"",
  },
  {
    title: "Conversations aren't queryable data",
    body: "Every review's transcript already lives in this datapack's conversation history -- but you can't run a GROUP BY over free-text chat.",
  },
  {
    title: "A dashboard needs structure",
    body: "Model, tokens, cost, findings -- one row per review, in real columns you can sum, average, and chart over time.",
  },
];

export function DashboardIntroStage() {
  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <div className="flex items-center gap-2">
        <Database className="h-5 w-5 text-primary" aria-hidden />
        <h2 className="text-lg font-semibold text-foreground">Why a dashboard needs real storage</h2>
      </div>
      <p className="text-sm text-muted-foreground">
        Every Meko datapack can back its own application storage — a real, fractional
        Postgres instance provisioned through AMP. It's yours, not Meko's: the same kind
        of database you'd reach for behind any app. Next, you'll create one for this
        datapack, then use it to track your own review runs.
      </p>
      <div className="grid gap-3 sm:grid-cols-3">
        {WHY_NOT_MEMORY.map(({ title, body }) => (
          <div key={title} className="rounded-xl border border-border bg-card p-4">
            <p className="text-sm font-semibold text-foreground">{title}</p>
            <p className="mt-1 text-xs text-muted-foreground">{body}</p>
          </div>
        ))}
      </div>
    </div>
  );
}
