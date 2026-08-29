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
        <Tabs defaultValue="overview">
          <TabsList>
            <TabsTrigger value="overview">Overview</TabsTrigger>
          </TabsList>
          <TabsContent value="overview" className="flex flex-col gap-4 pt-4">
            <p className="text-sm text-muted-foreground">{lab.description}</p>
            <ol className="flex flex-col gap-2">
              {lab.chapters.map((chapter, i) => (
                <li key={chapter.id} className="text-sm">
                  <span className="font-medium">{i + 1}. {chapter.title}</span>
                </li>
              ))}
            </ol>
          </TabsContent>
        </Tabs>
      </div>
    </div>
  );
}
