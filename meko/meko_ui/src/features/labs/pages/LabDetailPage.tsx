// A lab's landing page: what it is, what it costs you in time, where it sits
// in the ADLC, and the one button that starts it.

import { Link, Navigate, useLocation } from "react-router-dom";

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { ROUTES, labRunnerPath } from "@/app/routes/paths";

import { AdlcStageStrip } from "../components/AdlcStageStrip";
import { LIFECYCLE_DEFINITIONS } from "../data/adlcStages";
import { getLabDefinition } from "../data/labDefinitions";
import { flattenLabStages } from "../lib/labChapters";
import { parseLabRoute } from "../lib/parseLabRoute";

export default function LabDetailPage() {
  const location = useLocation();
  const parsed = parseLabRoute(location.pathname);
  const lab = parsed ? getLabDefinition(parsed.labId) : undefined;

  if (!lab) return <Navigate to={ROUTES.labs} replace />;

  const Icon = lab.icon;
  const stageCount = flattenLabStages(lab.chapters).length;

  return (
    <div className="flex h-full min-h-0 flex-col overflow-y-auto">
      <AppWorkspaceHeader
        leading={
          <Link to={ROUTES.labs} className="text-sm text-muted-foreground hover:text-foreground">
            &larr; All labs
          </Link>
        }
      />

      <div className="mx-auto flex w-full max-w-[1400px] flex-col gap-8 px-8 py-10">
        <div className="flex flex-col items-start gap-4">
          <div className="rounded-lg bg-primary/10 p-3">
            <Icon className="h-7 w-7 text-primary" aria-hidden />
          </div>
          <h1 className="text-3xl font-semibold">{lab.title}</h1>
          {/* The two lifecycles, a tile each — the framing a visitor needs
              before the lab makes sense. Global, not per-lab, so it comes from
              adlcStages rather than the LabDefinition. */}
          <div className="grid w-full max-w-5xl grid-cols-1 gap-4 md:grid-cols-2">
            {LIFECYCLE_DEFINITIONS.map((entry) => (
              <Card key={entry.term} className="flex h-full flex-col gap-3 p-5">
                <div>
                  <p className="text-lg font-semibold leading-tight text-foreground">
                    {entry.term}
                  </p>
                  <p className="mt-0.5 text-xs uppercase tracking-wide text-muted-foreground">
                    {entry.expansion}
                  </p>
                </div>
                <p className="text-sm text-muted-foreground">{entry.body}</p>
                {/* The context line carries the argument, so it gets the accent
                    rule and the brighter text rather than trailing the body. */}
                <p className="mt-auto border-l-2 border-primary/70 pl-3 text-sm text-foreground">
                  {entry.context}
                </p>
              </Card>
            ))}
          </div>

          {/* The question a visitor actually has before clicking Launch, so it
              sits directly above the button. Derived from the lab's own
              definition rather than restated prose that can drift from it. */}
          <p className="flex flex-wrap items-center gap-x-2 gap-y-1 text-base text-muted-foreground">
            <span className="font-semibold text-foreground">~{lab.estimatedMinutes} minutes</span>
            <span aria-hidden>·</span>
            <span className="font-semibold text-foreground">{lab.chapters.length} chapters</span>
            <span aria-hidden>·</span>
            <span className="text-sm">{stageCount} steps</span>
            <span aria-hidden className="text-sm">
              ·
            </span>
            <span className="text-sm">Nothing to install</span>
          </p>

          <div className="flex flex-wrap gap-1.5">
            {lab.tags.map((tag) => (
              <Badge key={tag} variant="secondary">
                {tag}
              </Badge>
            ))}
          </div>
          <Button asChild size="lg">
            <Link to={labRunnerPath(lab.id)}>Launch lab</Link>
          </Button>
        </div>

        {/* Which stage of the lifecycle this lab covers, and what is still to
            come. Per-lab, which is why it lives here and not on the index. */}
        <div className="flex flex-col gap-2">
          <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Where this sits in the ADLC
          </p>
          <AdlcStageStrip />
        </div>
      </div>
    </div>
  );
}
