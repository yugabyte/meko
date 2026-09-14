// A lab's landing page: what it is, what running it involves chapter by
// chapter, and the one button that starts it.

import { Link, Navigate, useLocation } from "react-router-dom";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { ROUTES, labRunnerPath } from "@/app/routes/paths";

import { LabChapterArc } from "../components/LabChapterArc";
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

      <div className="mx-auto flex w-full max-w-6xl flex-col gap-8 px-6 py-10">
        <div className="flex flex-col items-start gap-4">
          <div className="rounded-lg bg-primary/10 p-3">
            <Icon className="h-7 w-7 text-primary" aria-hidden />
          </div>
          <h1 className="text-3xl font-semibold">{lab.title}</h1>
          <p className="max-w-2xl text-base text-muted-foreground">{lab.description}</p>
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

        <LabChapterArc chapters={lab.chapters} />

        {/* Facts a visitor weighs before starting, derived from the lab's own
            definition rather than restated prose that can drift from it. */}
        <p className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
          <span>~{lab.estimatedMinutes} minutes</span>
          <span aria-hidden>·</span>
          <span>
            {lab.chapters.length} chapters, {stageCount} steps
          </span>
          <span aria-hidden>·</span>
          <span>Nothing to install</span>
          <span aria-hidden>·</span>
          <span>{lab.demoApp.agentDisplayName} runs on AWS Bedrock AgentCore</span>
        </p>
      </div>
    </div>
  );
}
