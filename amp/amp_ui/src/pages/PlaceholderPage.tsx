// Generic centre-body page reused by every not-yet-built nav target
// (Foundations, Fleet, agents, …). Keeps the shell exercisable across
// routes without one bespoke file per section.

export default function PlaceholderPage({ title }: { title: string }) {
  return (
    <div className="mx-auto max-w-5xl px-6 py-10">
      <h1 className="text-2xl font-bold tracking-tight">{title}</h1>
      <p className="mt-2 text-muted-foreground">
        This section is coming soon.
      </p>
    </div>
  );
}
