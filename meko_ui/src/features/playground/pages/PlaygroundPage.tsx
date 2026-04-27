import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";

/**
 * Lightweight area for trying Meko with the default datapack (placeholder until wired).
 */
export default function PlaygroundPage() {
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden bg-background text-foreground">
      <AppWorkspaceHeader
        leading={
          <div>
            <h1 className="text-xl font-semibold tracking-tight">Playground</h1>
            <p className="text-sm text-muted-foreground">
              Sample flows and uploads will live here.
            </p>
          </div>
        }
        showCliButton={false}
      />
      <div className="flex flex-1 flex-col items-center justify-center px-2 py-6 text-center sm:px-6 sm:py-10">
        <div className="relative w-full overflow-hidden rounded-xl border border-border/70 bg-[#0E1525]/70 shadow-xl">
          <img
            src="/playground-coming-soon.png"
            alt="Playground preview"
            className="h-auto w-full scale-[1.06] object-cover origin-center"
          />
          <div className="absolute inset-0 bg-gradient-to-b from-black/35 via-black/55 to-black/35" />
          <div className="absolute inset-0 flex items-center justify-center px-4 sm:px-8">
            <div className="w-full max-w-xl rounded-2xl border border-white/15 bg-black/45 px-5 py-5 shadow-2xl backdrop-blur-md sm:px-7 sm:py-6">
              <p className="text-lg font-semibold tracking-tight text-white sm:text-xl">Coming soon</p>
              <p className="mt-2 text-sm leading-relaxed text-white/85">
                Guided playground flows with sample knowledge, uploads, and chat are on the way.
              </p>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
