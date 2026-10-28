// The left rail: the Meko mark, the Status link, the Extensions group, the datapacks, and the
// version (with a chip to the Status page's Updates card when a newer release is out), settings and
// theme toggle. Below md it's a drawer over the page, opened from the top bar. While Meko Cloud is
// linked, the datapacks are one list: this Meko's (a laptop) and then Meko Cloud's (a cloud). The
// Find a datapack box above them narrows both to the names that hold what's typed, like Meko Cloud's.

import { useState } from "react";
import { Database, Laptop, Moon, Plus, RefreshCw, Search, Settings, Sun, X } from "lucide-react";
import { isCloudFailure, matchesName, useCloudDatapacks } from "../lib/cloud";
import { href, navigate, type Route } from "../lib/route";
import type { Theme } from "../lib/theme";
import { useUpdateAvailable } from "../lib/update";
import type { AuthStatus, Datapack } from "../lib/types";
import { CloudDatapackRows } from "./CloudDatapackRows";
import { DatapackDialog } from "./DatapackDialog";
import { ExtensionsLink } from "./ExtensionsLink";
import { SettingsDialog } from "./SettingsDialog";
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
  activeId,
  view,
  auth,
  version,
  theme,
  onToggleTheme,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  datapacks: Datapack[];
  activeId?: string;
  view: Route["view"];
  auth: AuthStatus;
  version?: string;
  theme: Theme;
  onToggleTheme: () => void;
  onCreated: (id: string) => void;
}) {
  const [creating, setCreating] = useState(false);
  const [settings, setSettings] = useState(false);
  const [query, setQuery] = useState("");
  const update = useUpdateAvailable();
  const cloud = useCloudDatapacks();
  const linked = !!cloud.data?.linked || isCloudFailure(cloud.error);
  const LocalIcon = linked ? Laptop : Database;
  const status = { tab: "overview" as const, view: "status" as const };
  const filtering = query.trim() !== "";
  const local = datapacks.filter((d) => matchesName(d.datapack_name, query));
  const cloudRows = linked ? (cloud.data?.datapacks ?? []) : [];
  // Nothing matched, and Meko Cloud's rows aren't something else instead: its first listing, which
  // may still bring matches (and until which `linked` can't be known), or its error note. A refresh
  // keeps the rows it had, so the query is matched against them.
  const noMatch =
    filtering &&
    local.length === 0 &&
    !(cloud.loading && !cloud.data) &&
    !(linked && cloud.error) &&
    !cloudRows.some((d) => matchesName(d.datapack_name, query));
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
        </div>

        <div className="flex items-center justify-between px-4 pb-1 pt-2">
          <span className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Datapacks</span>
          <div className="flex items-center">
            {linked && (
              <Button
                size="sm"
                variant="ghost"
                onClick={cloud.reload}
                disabled={cloud.loading}
                title="Refresh Meko Cloud's datapacks"
              >
                <RefreshCw className={cx("size-3.5", cloud.loading && "animate-spin")} />
              </Button>
            )}
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setCreating(true)}
              title={linked ? "New datapack on this computer" : "New datapack"}
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
          {local.map((d) => (
            <a
              key={d.datapack_id}
              href={href({ datapackId: d.datapack_id, tab: "overview" })}
              onClick={(e) => {
                e.preventDefault();
                navigate({ datapackId: d.datapack_id, tab: "overview" });
              }}
              title={linked ? "On this computer" : undefined}
              className={cx(
                "flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm",
                d.datapack_id === activeId ? "bg-accent font-medium" : "text-muted-foreground hover:bg-accent",
              )}
            >
              <LocalIcon
                className={cx("size-4 shrink-0", d.datapack_id === activeId && "text-primary")}
                aria-hidden
              />
              <span className="truncate">{d.datapack_name}</span>
              {linked && <span className="sr-only">, on this computer</span>}
            </a>
          ))}
          {linked && (
            <CloudDatapackRows
              datapacks={cloud.data?.datapacks ?? []}
              loading={cloud.loading}
              error={cloud.error}
              query={query}
            />
          )}
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
            <Button size="sm" variant="ghost" onClick={() => setSettings(true)} title="Settings">
              <Settings className="size-3.5" />
            </Button>
            <Button size="sm" variant="ghost" onClick={onToggleTheme} title="Toggle theme">
              {theme === "dark" ? <Sun className="size-3.5" /> : <Moon className="size-3.5" />}
            </Button>
          </div>
        </div>
      </aside>
      {settings && <SettingsDialog auth={auth} onClose={() => setSettings(false)} />}

      {creating && (
        <DatapackDialog
          onClose={() => setCreating(false)}
          onSaved={(id) => {
            setCreating(false);
            onCreated(id);
          }}
        />
      )}
    </>
  );
}
