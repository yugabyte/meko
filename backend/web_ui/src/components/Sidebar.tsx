// The left rail: the Meko mark, the Status link, the Extensions group, Projects, the Settings link,
// the datapacks, and the version (with a chip to the Status page's Updates card when a newer release is
// out) and theme toggle. Below md it's a drawer over the page, opened from the top bar. The datapacks
// are one list, by name, each opened where its row's web_url says: this UI's pages, or Meko Cloud's
// web app in a new tab. The Find a datapack box above them narrows it to the names that hold what's
// typed, like Meko Cloud's.

import { useState } from "react";
import { Database, ExternalLink, Moon, Plus, RefreshCw, Search, Sun, Users, X } from "lucide-react";
import { ApiError } from "../lib/api";
import { isShared, matchesName, ROWS_SHOWN, rowLink, useCloudLinked } from "../lib/cloud";
import { href, navigate, type Route } from "../lib/route";
import type { Theme } from "../lib/theme";
import { useUpdateAvailable } from "../lib/update";
import type { Datapack } from "../lib/types";
import { DatapackDialog } from "./DatapackDialog";
import { ExtensionsLink } from "./ExtensionsLink";
import { ProjectsLink } from "./ProjectsLink";
import { SettingsLink } from "./SettingsLink";
import { StatusLink } from "./StatusLink";
import { Button, cx, Input } from "./ui";

// The Meko mark and the Local tag.
export function Logo({ theme }: { theme: Theme }) {
  return (
    <div className="flex items-center gap-2">
      <img src={theme === "dark" ? "/meko-logo-white.svg" : "/meko-logo.svg"} alt="Meko" className="h-6" />
      <span className="rounded bg-primary/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-primary">
        Local
      </span>
    </div>
  );
}

export function Sidebar({
  open,
  onClose,
  datapacks,
  loading,
  error,
  onRefresh,
  activeId,
  view,
  version,
  theme,
  onToggleTheme,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  datapacks: Datapack[];
  loading: boolean;
  error?: Error;
  onRefresh: () => void;
  activeId?: string;
  view: Route["view"];
  version?: string;
  theme: Theme;
  onToggleTheme: () => void;
  // open says whether the new datapack opens here (it was made on this computer).
  onCreated: (id: string, open: boolean) => void;
}) {
  const [creating, setCreating] = useState(false);
  const [query, setQuery] = useState("");
  const update = useUpdateAvailable();
  const linked = !!useCloudLinked().data;
  const status = { tab: "overview" as const, view: "status" as const };
  const filtering = query.trim() !== "";
  const [all, setAll] = useState(false);
  const matches = datapacks.filter((d) => matchesName(d.datapack_name, query));
  // A match is the row being looked for, so none waits behind Show all.
  const shown = filtering || all ? matches : matches.slice(0, ROWS_SHOWN);
  const noMatch = filtering && matches.length === 0 && !loading && !error;
  return (
    <>
      {open && <div className="fixed inset-0 z-40 bg-black/50 md:hidden" onClick={onClose} />}
      <aside
        className={cx(
          "fixed inset-y-0 left-0 z-50 flex w-64 shrink-0 flex-col border-r bg-sidebar transition-transform md:static md:w-60 md:translate-x-0",
          // Closed, it's also invisible below md, so its links leave the tab order and the accessibility tree.
          open ? "translate-x-0" : "invisible -translate-x-full md:visible",
        )}
      >
        <div className="flex items-center justify-between px-4 py-4">
          <Logo theme={theme} />
          <Button variant="ghost" size="sm" onClick={onClose} aria-label="Close menu" className="md:hidden">
            <X className="size-4" />
          </Button>
        </div>
        <div className="px-2 pb-2">
          <StatusLink active={view === "status"} />
          <ExtensionsLink active={view === "extensions"} />
          <ProjectsLink active={view === "projects" || view === "project"} />
          <SettingsLink active={view === "settings"} />
        </div>

        <div className="flex items-center justify-between px-4 pb-1 pt-2">
          <span className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Datapacks</span>
          <div className="flex items-center">
            {linked && (
              <Button size="sm" variant="ghost" onClick={onRefresh} disabled={loading} title="Refresh the datapacks">
                <RefreshCw className={cx("size-3.5", loading && "animate-spin")} />
              </Button>
            )}
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setCreating(true)}
              title={linked ? "New datapack in Meko Cloud" : "New datapack"}
            >
              <Plus className="size-3.5" />
            </Button>
          </div>
        </div>
        <div className="relative px-2 pb-1">
          <Search
            className="pointer-events-none absolute left-4 top-1/2 size-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            type="search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Escape") setQuery("");
            }}
            placeholder="Find a datapack…"
            aria-label="Find a datapack"
            className="pl-8 pr-8 [&::-webkit-search-cancel-button]:hidden"
          />
          {query && (
            <button
              type="button"
              onClick={() => setQuery("")}
              aria-label="Clear the search"
              title="Clear"
              className="absolute right-4 top-1/2 -translate-y-1/2 rounded text-muted-foreground hover:text-foreground"
            >
              <X className="size-4" />
            </button>
          )}
        </div>
        <nav className="flex-1 space-y-0.5 overflow-y-auto px-2">
          {shown.map((d) => (
            <DatapackRow key={d.datapack_id} d={d} active={d.datapack_id === activeId} />
          ))}
          {!filtering && matches.length > ROWS_SHOWN && (
            <button
              type="button"
              aria-expanded={all}
              onClick={() => setAll(!all)}
              className="w-full rounded-lg px-2 py-1.5 text-left text-xs font-medium text-primary hover:bg-accent"
            >
              {all ? "Show fewer" : `Show all ${matches.length}`}
            </button>
          )}
          {error && <ListError error={error} />}
          {noMatch && (
            <div className="px-2 py-1.5 text-sm italic text-muted-foreground">
              No datapacks match “{query.trim()}”
            </div>
          )}
        </nav>

        <div className="flex items-center justify-between border-t px-4 py-3 text-xs text-muted-foreground">
          <span className="flex min-w-0 items-center gap-1.5">
            <span className="truncate">Meko {version ?? ""}</span>
            {update && (
              <a
                href={href(status)}
                onClick={(e) => (e.preventDefault(), navigate(status))}
                title={`Meko ${update.latest} is available`}
                className="shrink-0 rounded bg-primary/15 px-1.5 py-0.5 text-[10px] font-semibold text-primary hover:bg-primary/25"
              >
                Update
              </a>
            )}
          </span>
          <div className="flex items-center">
            <Button size="sm" variant="ghost" onClick={onToggleTheme} title="Toggle theme">
              {theme === "dark" ? <Sun className="size-3.5" /> : <Moon className="size-3.5" />}
            </Button>
          </div>
        </div>
      </aside>

      {creating && (
        <DatapackDialog
          inCloud={linked}
          onClose={() => setCreating(false)}
          onSaved={(id, created) => {
            setCreating(false);
            // Meko decides where it's made, and its reply's web_url says where it opens: one made in
            // Meko Cloud shows in the list, and opens in Meko Cloud's web app.
            onCreated(id, !!created && rowLink(created).kind === "here");
          }}
        />
      )}
    </>
  );
}

const ROW = "flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm";

// A datapack's row: a link to its pages here, a link to Meko Cloud's web app in a new tab, or, when
// that web app isn't known, a row that opens nothing. One shared with the user ends with Meko Cloud's
// two-people icon.
function DatapackRow({ d, active }: { d: Datapack; active: boolean }) {
  const link = rowLink(d);
  const body = (
    <>
      <Database className={cx("size-4 shrink-0", active && "text-primary")} aria-hidden />
      <span className="min-w-0 flex-1 truncate">{d.datapack_name}</span>
      {isShared(d.grant) && (
        <span title="Shared with you" className="shrink-0">
          <Users className="size-3.5 text-primary" aria-hidden />
          <span className="sr-only">, shared with you</span>
        </span>
      )}
      {link.kind === "web" && (
        <>
          <ExternalLink className="size-3.5 shrink-0" aria-hidden />
          <span className="sr-only">, opens in a new tab</span>
        </>
      )}
    </>
  );
  if (link.kind === "here") {
    const to = { datapackId: d.datapack_id, tab: "overview" as const };
    return (
      <a
        href={href(to)}
        onClick={(e) => (e.preventDefault(), navigate(to))}
        aria-current={active ? "page" : undefined}
        className={cx(ROW, active ? "bg-accent font-medium" : "text-muted-foreground hover:bg-accent")}
      >
        {body}
      </a>
    );
  }
  if (link.kind === "web") {
    return (
      <a
        href={link.url}
        target="_blank"
        rel="noopener noreferrer"
        title="Opens in a new tab"
        className={cx(ROW, "text-muted-foreground hover:bg-accent")}
      >
        {body}
      </a>
    );
  }
  return (
    <div
      aria-disabled="true"
      title="Can't be opened from here. Set the web app URL in Settings › Meko Cloud to open it"
      className={cx(ROW, "text-muted-foreground opacity-60")}
    >
      {body}
    </div>
  );
}

const settings = { tab: "overview" as const, view: "meko-cloud" as const };

// The codes whose fix is on Settings › Meko Cloud.
const FIX_IN_SETTINGS = new Set(["CLOUD_KEY_REFUSED", "CLOUD_NO_API_URL"]);

// A listing that failed: one note, linking to Settings › Meko Cloud when the fix is there.
function ListError({ error }: { error: Error }) {
  const fix = error instanceof ApiError && FIX_IN_SETTINGS.has(error.code);
  return (
    <div className="mx-2 my-1 rounded-lg bg-destructive/10 px-2 py-1.5 text-xs text-destructive">
      {error.message}
      {fix && (
        <>
          {" "}
          <a
            href={href(settings)}
            onClick={(e) => (e.preventDefault(), navigate(settings))}
            className="font-medium underline"
          >
            Open Meko Cloud settings
          </a>
        </>
      )}
    </div>
  );
}
