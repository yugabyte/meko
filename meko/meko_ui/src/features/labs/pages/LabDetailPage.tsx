import { ArrowRight } from "lucide-react";
import { Link, Navigate, useLocation } from "react-router-dom";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { ROUTES, labRunnerPath } from "@/app/routes/paths";

import { getLabDefinition } from "../data/labDefinitions";
import { parseLabRoute } from "../lib/parseLabRoute";

export default function LabDetailPage() {
  const location = useLocation();
  const parsed = parseLabRoute(location.pathname);
  const lab = parsed ? getLabDefinition(parsed.labId) : undefined;

  if (!lab) return <Navigate to={ROUTES.labs} replace />;

  const Icon = lab.icon;

  return (
    <div className="flex h-full min-h-0 flex-col overflow-y-auto">
      <AppWorkspaceHeader
        leading={
          <Link to={ROUTES.labs} className="text-sm text-muted-foreground hover:text-foreground">
            &larr; Labs
          </Link>
        }
      />
      <div className="mx-auto flex w-full max-w-2xl flex-col items-center gap-4 px-6 py-10 text-center">
        <div className="rounded-md bg-primary/10 p-3">
          <Icon className="h-8 w-8 text-primary" />
        </div>
        <h1 className="text-2xl font-semibold">{lab.title}</h1>
        <p className="text-sm text-muted-foreground">{lab.description}</p>
        <div className="flex flex-wrap justify-center gap-1.5">
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
      <div className="mx-auto w-full max-w-3xl px-6 pb-10">
        {/* How this lab starts, in three boxes -- replaces a paragraph of
            prose with something scannable at a glance. */}
        <div className="mb-8 flex items-stretch gap-2">
          {lab.startingPoint.map((step, i) => (
            <div key={step.title} className="flex flex-1 items-stretch gap-2">
              <div className="flex-1 rounded-lg bg-muted/50 p-3 text-center">
                <p className="text-sm font-medium">{step.title}</p>
                <p className="mt-1 text-xs text-muted-foreground">{step.subtitle}</p>
              </div>
              {i < lab.startingPoint.length - 1 && (
                <div className="flex shrink-0 items-center text-muted-foreground" aria-hidden>
                  <ArrowRight className="h-4 w-4" />
                </div>
              )}
            </div>
          ))}
        </div>

        {/* Chapters only, not individual stages -- this lab has 2 today but
            more are coming, and only the selected chapter's summary shows
            at once instead of stacking every chapter's text on the page. */}
        <Tabs defaultValue={lab.chapters[0].id}>
          <TabsList>
            {lab.chapters.map((chapter, i) => (
              <TabsTrigger key={chapter.id} value={chapter.id}>
                {i + 1}. {chapter.title}
              </TabsTrigger>
            ))}
          </TabsList>
          {lab.chapters.map((chapter) => (
            <TabsContent key={chapter.id} value={chapter.id} className="pt-4">
              <p className="text-sm text-muted-foreground">{chapter.summary}</p>
            </TabsContent>
          ))}
        </Tabs>
      </div>
    </div>
  );
}
