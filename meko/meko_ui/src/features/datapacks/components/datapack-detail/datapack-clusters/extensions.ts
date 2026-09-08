// The AMP cluster engine + extension catalog, mirrored for the create-cluster
// wizard. This is a hardcoded copy of the controller's catalog (source of truth:
// amp/deploy/extensions/extensions.yaml → amp/controller/internal/extensions).
// amp_ui generates the same data into src/config/extensions.json; Meko keeps a
// small constant instead of the generation pipeline — the catalog is tiny and
// stable, and the controller rejects any unknown engine/extension anyway.

export type AmpExtension = {
  name: string;
  version: string;
  description: string;
};

// The extension catalog (the only accepted `extensions[]` values on the wire).
export const AMP_EXTENSIONS: AmpExtension[] = [
  { name: "pgvector", version: "0.8.2", description: "Vector similarity search" },
  { name: "pg_cron", version: "1.6.7", description: "Cron-style job scheduler" },
  { name: "hexabridge", version: "0.1", description: "Oracle compatibility (DBMS_*/UTL_* packages)" },
  { name: "age", version: "1.8.0", description: "Apache AGE — openCypher graph" },
];

export type DbEngine = {
  id: string;
  label: string;
  sub?: string; // secondary label, e.g. the Postgres major version
  comingSoon?: boolean;
};

// Selectable engines map to a controller engine bundle; sqlserver/documentdb are
// roadmap tiles shown disabled so the direction is visible without implying support.
export const DB_ENGINES: DbEngine[] = [
  { id: "postgres", label: "PostgreSQL", sub: "18" },
  { id: "aidb", label: "AI Database" },
  { id: "oracle", label: "Oracle" },
  { id: "sqlserver", label: "SQL Server", comingSoon: true },
  { id: "documentdb", label: "DocumentDB", comingSoon: true },
];

// Engine → the extensions its bundle seeds on the default branch. The controller
// unions these with the request's `extensions`, so the wizard shows them locked
// (already-on) and sends only the user's extra picks.
const ENGINE_EXTENSIONS: Record<string, string[]> = {
  postgres: [],
  aidb: ["pgvector", "age", "pg_cron"],
  oracle: ["hexabridge"],
};

export function engineExtensions(engine: string): string[] {
  return ENGINE_EXTENSIONS[engine] ?? [];
}

export function engineLabel(engine: string): string {
  const e = DB_ENGINES.find((x) => x.id === engine);
  if (!e) return engine;
  return e.sub ? `${e.label} ${e.sub}` : e.label;
}
