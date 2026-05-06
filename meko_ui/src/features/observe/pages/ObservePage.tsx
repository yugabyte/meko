import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  bootstrapObserveSession,
  DATAPACKS_LIST_UPDATED_EVENT,
  ensureLangfuseProjectKeys,
  getApiErrorMessage,
  listDatapacks,
} from "@/lib/mekoApi";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import {
  clearObserveBootstrapCache,
  getObserveIframeVersion,
  isObserveBootstrapFresh,
  OBSERVE_IFRAME_VERSION_STORAGE_KEY,
  readObserveBootstrapCache,
  writeObserveBootstrapCache,
} from "@/features/observe/observeRefresh";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";

const OBSERVE_EMBED_PATH_RAW = (import.meta.env.VITE_OBSERVE_EMBED_PATH || "").trim();

function normalizeObserveEmbedPath(rawPath: string): string {
  const value = rawPath.trim();
  if (!value) return "";
  if (value.startsWith("/")) {
    return value.replace(/^\/observe\/embed\/?/, "/observe/");
  }
  if (value.startsWith("http://localhost:3000")) {
    return value.replace("http://localhost:3000", "").replace(/^\/observe\/embed\/?/, "/observe/");
  }
  if (value.startsWith("https://localhost:3000")) {
    return value.replace("https://localhost:3000", "").replace(/^\/observe\/embed\/?/, "/observe/");
  }
  try {
    const parsed = new URL(value);
    const path = `${parsed.pathname}${parsed.search}${parsed.hash}`;
    return path.replace(/^\/observe\/embed\/?/, "/observe/");
  } catch {
    return value.replace(/^\/observe\/embed\/?/, "/observe/");
  }
}

/**
 * Iframe lives under Langfuse basePath (/observe/...) on meko-proxy :5000 so Next.js router matches.
 * Keep a trailing slash on the base when there is no extra path (nginx / location matching).
 */
function observeIframeSrc(embedBase: string, langfusePath: string, retry: number): string {
  const base = embedBase.replace(/\/+$/, "");
  const path = langfusePath.replace(/^\/+/, "");
  const beforeQuery = path ? `${base}/${path}` : `${base}/`;
  const sep = beforeQuery.includes("?") ? "&" : "?";
  return `${beforeQuery}${sep}r=${retry}`;
}

export default function ObservePage() {
  const [bootstrapping, setBootstrapping] = useState(true);
  const [error, setError] = useState<string | null>(null);
  /**
   * Iframe-version counter, embedded in the iframe `src` as `?r=<n>` so a
   * change forces both the browser HTTP-cache miss and a fresh Langfuse
   * SPA load (so the project list inside the iframe is re-fetched).
   *
   * Initialized from the persisted localStorage counter (bumped by
   * {@link notifyDatapacksListChanged}) so a datapack mutation that
   * happened on a different page before the user navigated here still
   * busts the iframe on this mount.
   */
  const [embedRetry, setEmbedRetry] = useState<number>(() => getObserveIframeVersion());
  /** Path under /observe/ without leading slash, e.g. organization/<id> or "". */
  const [langfuseRelPath, setLangfuseRelPath] = useState("");
  const observeEmbedPath = normalizeObserveEmbedPath(OBSERVE_EMBED_PATH_RAW);
  const [searchParams] = useSearchParams();

  /**
   * Optional deep-link query params written by other panels (e.g. the
   * datapack Conversations panel's "Open in Observe" button):
   *   ?project=<langfuse_project_id>&session=<session_id>
   *   ?project=<langfuse_project_id>&trace=<trace_id>
   *
   * Meko's MCP server writes one Langfuse trace per conversation with
   * trace.id == trace.session_id == Meko conversation_id (see
   * api_server/internal/repository/langfuse_repo.go:297-299), so both
   * params resolve to the same trace detail page.
   */
  const deepLinkRelPath = useMemo(() => {
    const projectId = (searchParams.get("project") || "").trim();
    if (!projectId) return "";
    const sessionId = (searchParams.get("session") || "").trim();
    if (sessionId) return `project/${projectId}/traces/${sessionId}`;
    const traceId = (searchParams.get("trace") || "").trim();
    if (traceId) return `project/${projectId}/traces/${traceId}`;
    return "";
  }, [searchParams]);

  const effectiveRelPath = deepLinkRelPath || langfuseRelPath;

  useEffect(() => {
    let mounted = true;
    if (!observeEmbedPath) {
      setError("Observe is not configured: missing VITE_OBSERVE_EMBED_PATH");
      setBootstrapping(false);
      return () => {
        mounted = false;
      };
    }

    const cached = readObserveBootstrapCache();
    if (cached && isObserveBootstrapFresh(cached.updatedAt)) {
      setLangfuseRelPath(cached.langfuseRelPath);
      setError(null);
      setBootstrapping(false);
      return () => {
        mounted = false;
      };
    }

    const prepareObserve = async () => {
      const datapacks = await listDatapacks();
      if (!datapacks.length) {
        throw new Error("No datapacks found for this account");
      }
      // listDatapacks() returns owned + shared datapacks. ensureLangfuseProjectKeys
      // hits an owner-only handler (POST /datapacks/<name>/langfuse/project-keys
      // → DatapackService.Describe → owner-only repo lookup), so we MUST pick an
      // owned datapack here. Without this filter, a user whose first datapack is
      // a share (sorted by created_at DESC) gets a 404 "datapack <name> not found"
      // and the iframe never loads. If the user has only shared datapacks, the
      // owner already provisioned the Langfuse project — we just bootstrap our
      // own org session and skip the keys call.
      const ownedDatapack = datapacks.find((dp) => (dp.grant ?? "owner") === "owner");
      if (ownedDatapack) {
        await ensureLangfuseProjectKeys(ownedDatapack.datapack_name);
      }
      const bootstrap = await bootstrapObserveSession();
      if (mounted && bootstrap.org_id) {
        const rel = `organization/${bootstrap.org_id}`;
        setLangfuseRelPath(rel);
        writeObserveBootstrapCache({
          langfuseRelPath: rel,
          updatedAt: Date.now(),
        });
      }
      await new Promise((resolve) => setTimeout(resolve, 250));
    };

    prepareObserve()
      .then(() => {
        if (!mounted) return;
        setError(null);
      })
      .catch((err) => {
        if (!mounted) return;
        setError(getApiErrorMessage(err, "Failed to load Observe."));
      })
      .finally(() => {
        if (!mounted) return;
        setBootstrapping(false);
      });

    return () => {
      mounted = false;
    };
  }, [observeEmbedPath]);

  /**
   * Keep the iframe src in sync with the persisted iframe-version counter.
   *
   *  - DATAPACKS_LIST_UPDATED_EVENT fires for same-tab mutations once
   *    {@link notifyDatapacksListChanged} has bumped the counter and cleared
   *    the in-flight session caches; we just re-read the counter and let the
   *    iframe `src` (and the `key` prop) change → Langfuse SPA reloads.
   *  - The native `storage` event fires for the SAME localStorage write in
   *    OTHER tabs, so an Observe tab open in tab B picks up a mutation made
   *    in tab A automatically.
   *  - Both listeners also clear the bootstrap cache defensively in case
   *    the helper version-skews and a stale tab is the one mutating.
   */
  useEffect(() => {
    const syncToPersistedVersion = (origin: string) => {
      const persisted = getObserveIframeVersion();
      void origin;
      clearObserveBootstrapCache();
      setEmbedRetry(persisted);
    };

    const onDatapacksChanged = () => syncToPersistedVersion("event");
    const onStorage = (e: StorageEvent) => {
      if (e.key !== OBSERVE_IFRAME_VERSION_STORAGE_KEY) return;
      syncToPersistedVersion("storage");
    };

    addAppEventListener(DATAPACKS_LIST_UPDATED_EVENT, onDatapacksChanged);
    globalThis.addEventListener("storage", onStorage);
    return () => {
      removeAppEventListener(DATAPACKS_LIST_UPDATED_EVENT, onDatapacksChanged);
      globalThis.removeEventListener("storage", onStorage);
    };
  }, []);

  return (
    <div className="flex-1 h-screen bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader
        padding="compact"
        sticky={false}
        showUserAvatar={false}
        leading={
          <div>
            <h1 className="text-lg font-semibold text-white">Observe</h1>
            <p className="text-xs text-muted-foreground">Langfuse traces embedded in Meko shell</p>
          </div>
        }
      />

      <div className="flex-1 min-h-0">
        {bootstrapping ? (
          <div className="w-full h-full flex items-center justify-center text-sm text-muted-foreground">
            Preparing Observe...
          </div>
        ) : error ? (
          <div className="w-full h-full flex items-center justify-center px-6 text-sm text-red-400">
            {error}
          </div>
        ) : (
          <iframe
            key={`${effectiveRelPath}#${embedRetry}`}
            title="Langfuse Observe"
            src={observeIframeSrc(observeEmbedPath, effectiveRelPath, embedRetry)}
            className="w-full h-full border-0 bg-[#0E1525]"
            onError={() => {
              if (embedRetry > 0) return;
              setError("Observe failed to load once, retrying...");
              setTimeout(() => {
                setError(null);
                setEmbedRetry(1);
              }, 400);
            }}
          />
        )}
      </div>
    </div>
  );
}
