import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";

import { LabCard } from "../components/LabCard";
import { LAB_DEFINITIONS } from "../data/labDefinitions";

export default function LabsListPage() {
  return (
    <div className="flex h-full min-h-0 flex-col overflow-y-auto">
      <AppWorkspaceHeader
        leading={
          <div>
            <h1 className="text-lg font-semibold">Labs</h1>
            <p className="text-sm text-muted-foreground">
              Try out guided labs that walk through a real use case built on Meko.
            </p>
          </div>
        }
      />
      <div className="grid grid-cols-1 gap-4 p-6 sm:grid-cols-2 lg:grid-cols-3">
        {LAB_DEFINITIONS.map((lab) => (
          <LabCard key={lab.id} lab={lab} />
        ))}
      </div>
    </div>
  );
}
