import { useEffect, useState } from "react";
import {
  bootstrapObserveSession,
  ensureLangfuseProjectKeys,
  getApiErrorMessage,
  listDatapacks,
} from "@/lib/mekoApi";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";

const OBSERVE_EMBED_PATH_RAW = (import.meta.env.VITE_OBSERVE_EMBED_PATH || "").trim();
const OBSERVE_BOOTSTRAP_CACHE_KEY = "meko:observe-bootstrap-cache:v1";
const OBSERVE_BOOTSTRAP_CACHE_TTL_MS = 5 * 60_000;

type ObserveBootstrapCache = {
  langfuseRelPath: string;
  updatedAt: number;
};

function readObserveBootstrapCache(): ObserveBootstrapCache | null {
  const raw = sessionStorage.getItem(OBSERVE_BOOTSTRAP_CACHE_KEY);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as ObserveBootstrapCache;
  } catch {
    sessionStorage.removeItem(OBSERVE_BOOTSTRAP_CACHE_KEY);
    return null;
  }
}

function writeObserveBootstrapCache(cache: ObserveBootstrapCache) {
  sessionStorage.setItem(OBSERVE_BOOTSTRAP_CACHE_KEY, JSON.stringify(cache));
}

function isObserveBootstrapFresh(updatedAt: number) {
  return Date.now() - updatedAt <= OBSERVE_BOOTSTRAP_CACHE_TTL_MS;
}

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
  const [embedRetry, setEmbedRetry] = useState(0);
  /** Path under /observe/ without leading slash, e.g. organization/<id> or "". */
  const [langfuseRelPath, setLangfuseRelPath] = useState("");
  const observeEmbedPath = normalizeObserveEmbedPath(OBSERVE_EMBED_PATH_RAW);

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
      await ensureLangfuseProjectKeys(datapacks[0].datapack_name);
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
            title="Langfuse Observe"
            src={observeIframeSrc(observeEmbedPath, langfuseRelPath, embedRetry)}
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
