import { Download } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { McpInstallGuide } from "@/features/datapacks/components/install/McpInstallGuide";

export default function HomePage() {
  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader showCliButton={false} />

      <main className="flex-1 space-y-8 p-8 pb-16 max-w-6xl mx-auto w-full">
        <div className="flex flex-col gap-2">
          <div className="flex items-center gap-3">
            <Download className="w-6 h-6 text-blue-400 shrink-0" aria-hidden />
            <h1 className="text-2xl font-semibold tracking-tight">Install</h1>
          </div>
          <p className="pl-9 text-sm text-muted-foreground max-w-2xl leading-relaxed">
            Choose your environment and follow the steps.
          </p>
        </div>

        <McpInstallGuide className="mt-8" />
      </main>
    </div>
  );
}
