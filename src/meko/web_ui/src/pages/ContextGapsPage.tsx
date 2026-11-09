// Context Gaps: the datapack's searches Meko answered poorly (found nothing, or only memories
// scoring below 0.75), grouped by question, latest asked first, 100 at a time. Each open one can be
// taught (saved as a memory here), copied or dismissed; a dismissed or taught one can be restored.
// A Meko Cloud datapack's (its searches pass through this Meko) has a page of its own at /gaps/<id>,
// CloudContextGapsPage: its conversations aren't on this Meko, so they aren't links there.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { Check, Copy, HelpCircle } from "lucide-react";
import { api } from "../lib/api";
import { href, navigate } from "../lib/route";
import { useLoad } from "../lib/useLoad";
import type { Datapack, Gap, GapPage, GapQuality, GapState } from "../lib/types";
import { Dialog } from "../components/Dialog";
import { Segmented } from "../components/Segmented";
import { Badge, Button, Card, Empty, ErrorNote, Field, Loading, Spinner } from "../components/ui";

type Filter = GapState | "all";

export const QUALITY_LABEL: Record<GapQuality, string> = { empty: "Found nothing", weak: "Nothing relevant" };

// The most a taught answer may be, as the server takes it.
export const TEACH_MAX = 2000;

// A query longer than this is cut on its row until it's opened.
const SHORT_QUERY = 240;

const day = (iso: string) => new Date(iso).toLocaleDateString([], { month: "short", day: "numeric" });
const short = (id: string) => id.slice(0, 8);
const asked = (n: number) => (n === 1 ? "Asked once" : `Asked ${n.toLocaleString()} times`);

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

// The datapack the page is for: its id and name are all it needs.
type GapsDatapack = Pick<Datapack, "datapack_id" | "datapack_name">;

// A Meko Cloud datapack's Context Gaps, named from the datapacks' listing (its id when that fails).
export function CloudContextGapsPage({ id }: { id: string }) {
  const { data } = useLoad(() => api.datapacks().catch((): Datapack[] => []), []);
  const name = data?.find((d) => d.datapack_id === id)?.datapack_name ?? id;
  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-xl font-semibold">Context Gaps</h1>
        <p className="text-sm text-muted-foreground">{name}, in Meko Cloud</p>
      </div>
      <ContextGapsPage datapack={{ datapack_id: id, datapack_name: name }} cloud />
    </div>
  );
}

const FILTERS: readonly Filter[] = ["open", "taught", "dismissed", "all"];

// routeFilter is the filter in the address (/datapacks/<id>/gaps/<filter>), so a link can open
// Taught directly; it sets the one the page starts on.
export function ContextGapsPage({
  datapack,
  cloud = false,
  routeFilter,
}: {
  datapack: GapsDatapack;
  cloud?: boolean;
  routeFilter?: string;
}) {
  const id = datapack.datapack_id;
  const [filter, setFilter] = useState<Filter>(() =>
    FILTERS.includes(routeFilter as Filter) ? (routeFilter as Filter) : "open",
  );
  const [page, setPage] = useState<GapPage>();
  const [gaps, setGaps] = useState<Gap[]>();
  const [error, setError] = useState<Error>();
  const [loading, setLoading] = useState(false);
  const [reload, setReload] = useState(0);
  const [busyKey, setBusyKey] = useState<string>();
  const [teaching, setTeaching] = useState<Gap>();
  const [copied, setCopied] = useState<string>();
  // Bumped whenever the list loads afresh, so a Load more answer for a list that's gone is dropped.
  const view = useRef(0);

  useEffect(() => {
    let gone = false;
    view.current++;
    setLoading(true);
    api
      .gaps(id, filter)
      .then((p) => {
        if (gone) return;
        setPage(p);
        setGaps(p.questions);
        setError(undefined);
      })
      .catch((e: Error) => !gone && setError(e))
      .finally(() => !gone && setLoading(false));
    return () => {
      gone = true;
    };
  }, [id, filter, reload]);

  const more = async () => {
    const last = gaps?.at(-1);
    if (!last) return;
    const mine = view.current;
    setLoading(true);
    try {
      const p = await api.gaps(id, filter, { last_at: last.last_at, key: last.key });
      if (view.current !== mine) return;
      setGaps((g) => [...(g ?? []), ...p.questions]);
      setPage((old) => (old ? { ...old, more: p.more } : p));
    } catch (e) {
      if (view.current === mine) setError(e as Error);
    } finally {
      if (view.current === mine) setLoading(false);
    }
  };

  const mark = async (g: Gap, state: "dismissed" | null) => {
    setBusyKey(g.key);
    setError(undefined);
    try {
      await api.markGap(id, g.key, state);
      setReload((n) => n + 1);
    } catch (e) {
      setError(e as Error);
    } finally {
      setBusyKey(undefined);
    }
  };

  const copy = async (text: string, what: string) => {
    if (await copyText(text)) {
      setCopied(what);
      setTimeout(() => setCopied(undefined), 1500);
    }
  };

  const filters: { id: Filter; label: string }[] = [
    { id: "open", label: "Open" },
    { id: "taught", label: "Taught" },
    { id: "dismissed", label: "Dismissed" },
    { id: "all", label: "All" },
  ].map((f) => ({
    id: f.id as Filter,
    label: page && f.id !== "all" ? `${f.label} ${page.counts[f.id as GapState].toLocaleString()}` : f.label,
  }));
  const open = page ? page.open.empty + page.open.weak : 0;

  return (
    <div className="space-y-4">
      <p className="max-w-3xl text-sm text-muted-foreground">
        Searches agents made in this datapack that found nothing, or found only memories scoring below 0.75. Teach Meko
        the answer, or dismiss the ones that don't need one.
      </p>
      {page && (
        <p className="text-sm text-muted-foreground">
          {page.since === null
            ? "No searches recorded yet."
            : `Since ${day(page.since)}: ${page.searches.toLocaleString()} ${page.searches === 1 ? "search" : "searches"}, ` +
              `${open.toLocaleString()} open ${open === 1 ? "question" : "questions"}`}
        </p>
      )}
      <div className="flex flex-wrap items-center gap-3">
        <Segmented options={filters} value={filter} onChange={setFilter} />
        {!!gaps?.length && (
          <Button
            size="sm"
            className="ml-auto"
            onClick={() => void copy(gaps.map((g) => `- ${g.query.replace(/\s+/g, " ")}`).join("\n"), "all")}
          >
            {copied === "all" ? <Check className="size-3.5 text-success" /> : <Copy className="size-3.5" />} Copy all
          </Button>
        )}
      </div>

      {error && <ErrorNote error={error} />}

      {!gaps ? (
        !error && <Loading />
      ) : !gaps.length ? (
        filter === "open" ? (
          <Empty icon={<HelpCircle className="size-8" />} title="Nothing Meko couldn't answer">
            When an agent's search here finds nothing, or nothing relevant, the question shows up here for you to teach
            Meko.
          </Empty>
        ) : (
          <p className="py-6 text-center text-sm text-muted-foreground">None.</p>
        )
      ) : (
        <Card className="divide-y">
          {gaps.map((g) => (
            <GapRow
              key={g.key}
              datapackId={id}
              cloud={cloud}
              gap={g}
              busy={busyKey === g.key}
              copied={copied === g.key}
              onTeach={() => setTeaching(g)}
              onCopy={() => void copy(g.query, g.key)}
              onMark={(state) => void mark(g, state)}
            />
          ))}
        </Card>
      )}
      {page?.more && (
        <div className="flex justify-center">
          <Button size="sm" disabled={loading} onClick={() => void more()}>
            {loading ? <Spinner /> : "Load more"}
          </Button>
        </div>
      )}

      {teaching && (
        <TeachDialog
          datapack={datapack}
          gap={teaching}
          onClose={() => setTeaching(undefined)}
          onTaught={() => {
            setTeaching(undefined);
            setReload((n) => n + 1);
          }}
        />
      )}
    </div>
  );
}

function GapRow({
  datapackId,
  cloud,
  gap: g,
  busy,
  copied,
  onTeach,
  onCopy,
  onMark,
}: {
  datapackId: string;
  cloud: boolean;
  gap: Gap;
  busy: boolean;
  copied: boolean;
  onTeach: () => void;
  onCopy: () => void;
  onMark: (state: "dismissed" | null) => void;
}) {
  const [full, setFull] = useState(false);
  const long = g.query.length > SHORT_QUERY;
  const conversation = (cid: string) => {
    if (cloud) {
      return (
        <span key={cid} title={cid} className="font-mono">
          {short(cid)}
        </span>
      );
    }
    const to = { datapackId, tab: "conversations" as const, itemId: cid };
    return (
      <a
        key={cid}
        href={href(to)}
        title={cid}
        className="font-mono text-primary hover:underline"
        onClick={(e) => {
          if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
          e.preventDefault();
          navigate(to);
        }}
      >
        {short(cid)}
      </a>
    );
  };
  // " by an agent in conversation <id>" for a mark an agent made; nothing for a person's.
  const byAgent = (cid: string | null): ReactNode => cid && <> by an agent in conversation {conversation(cid)}</>;
  const details = [
    g.tool,
    g.quality === "weak" && g.results !== null && `${g.results.toLocaleString()} found`,
    // The best score and the best cosine may be different memories'.
    g.top_score !== null && `best score ${g.top_score.toFixed(2)}`,
    g.top_raw !== null && `best cosine ${g.top_raw.toFixed(2)}`,
  ].filter(Boolean);

  return (
    <div className="space-y-2 p-3">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <p className="min-w-0 flex-1 whitespace-pre-wrap break-words text-sm">
          {long && !full ? `${g.query.slice(0, SHORT_QUERY)}…` : g.query}
        </p>
        <div className="flex shrink-0 gap-1">
          <Badge tone={g.quality === "empty" ? "danger" : "warning"}>{QUALITY_LABEL[g.quality]}</Badge>
          {g.state !== "open" && (
            <Badge tone={g.state === "taught" ? "success" : "neutral"}>
              {g.state === "taught" ? "Taught" : "Dismissed"}
            </Badge>
          )}
        </div>
      </div>
      <p className="text-xs text-muted-foreground">
        {asked(g.asks)}
        {g.agents.length > 0 && ` by ${g.agents.join(", ")}`}
        {g.conversations.length > 0 && (
          <>
            {" · in "}
            {g.conversations.map((cid, i) => (
              <span key={cid}>
                {i > 0 && ", "}
                {conversation(cid)}
              </span>
            ))}
          </>
        )}
        {` · ${g.asks > 1 ? "last " : ""}${day(g.last_at)}`}
        {details.length > 0 && ` · ${details.join(" · ")}`}
      </p>
      {g.taught && (
        <div className="space-y-1 rounded-lg bg-muted p-2 text-sm">
          <p className="text-xs text-muted-foreground">
            {/* Asked again only when its last ask came after the teaching: a restore or a dismissal
                since keeps what was taught without anyone asking again. */}
            {g.state === "open" && Date.parse(g.last_at) > Date.parse(g.taught.at) ? (
              <>
                Taught{byAgent(g.taught.conversation_id)} on {day(g.taught.at)}, and asked again since. Meko had this and
                the search still missed it:
              </>
            ) : (
              <>
                Taught{byAgent(g.taught.conversation_id)} on {day(g.taught.at)}:
              </>
            )}
          </p>
          <p className="whitespace-pre-wrap break-words">{g.taught.text}</p>
        </div>
      )}
      {g.state === "dismissed" && g.dismissal?.conversation_id && (
        <div className="space-y-1 rounded-lg bg-muted p-2 text-xs">
          <p className="text-muted-foreground">
            Dismissed{byAgent(g.dismissal.conversation_id)}
            {g.marked_at && ` on ${day(g.marked_at)}`}: Meko already had this
            {g.dismissal.memory_text ? ":" : g.dismissal.memory_id ? (
              <>
                {" in memory "}
                <span className="font-mono" title={g.dismissal.memory_id}>
                  {short(g.dismissal.memory_id)}
                </span>
              </>
            ) : null}
          </p>
          {g.dismissal.memory_text && <p className="whitespace-pre-wrap break-words">{g.dismissal.memory_text}</p>}
        </div>
      )}
      <div className="flex flex-wrap items-center gap-2">
        {long && (
          <Button size="sm" variant="ghost" aria-expanded={full} onClick={() => setFull(!full)}>
            {full ? "Show less" : "Show all"}
          </Button>
        )}
        {g.state === "open" && (
          <Button size="sm" variant="primary" onClick={onTeach}>
            Teach
          </Button>
        )}
        <Button size="sm" onClick={onCopy}>
          {copied ? <Check className="size-3.5 text-success" /> : <Copy className="size-3.5" />} Copy
        </Button>
        {g.state === "open" ? (
          <Button size="sm" disabled={busy} onClick={() => onMark("dismissed")}>
            {busy ? <Spinner className="size-3.5" /> : "Dismiss"}
          </Button>
        ) : (
          <Button size="sm" disabled={busy} onClick={() => onMark(null)}>
            {busy ? <Spinner className="size-3.5" /> : "Restore"}
          </Button>
        )}
      </div>
    </div>
  );
}

// What Meko should know about a question, saved as a memory in the datapack. A failed save keeps the
// text and says why.
function TeachDialog({
  datapack,
  gap,
  onClose,
  onTaught,
}: {
  datapack: GapsDatapack;
  gap: Gap;
  onClose: () => void;
  onTaught: (g: Gap) => void;
}) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();
  const trimmed = text.trim();
  const save = async () => {
    if (!trimmed || busy) return;
    setBusy(true);
    setError(undefined);
    try {
      onTaught(await api.teachGap(datapack.datapack_id, gap.key, trimmed));
    } catch (e) {
      setError(e as Error);
      setBusy(false);
    }
  };
  return (
    <Dialog
      title="Teach Meko"
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!trimmed || busy} onClick={() => void save()}>
            {busy ? <Spinner className="size-3.5" /> : "Save as a memory"}
          </Button>
        </>
      }
    >
      <div className="space-y-1">
        <p className="text-xs text-muted-foreground">
          {asked(gap.asks)}, {QUALITY_LABEL[gap.quality].toLowerCase()}:
        </p>
        <blockquote className="whitespace-pre-wrap break-words border-l-2 pl-3 text-sm">{gap.query}</blockquote>
      </div>
      <Field
        label="What should Meko know?"
        hint={`Saved as a memory in ${datapack.datapack_name}. Write it so it makes sense on its own.`}
      >
        <textarea
          id="teach-text"
          autoFocus
          rows={6}
          maxLength={TEACH_MAX}
          value={text}
          onChange={(e) => setText(e.target.value)}
          className="w-full rounded-lg border bg-card px-3 py-2 text-base outline-none sm:text-sm focus:border-primary"
        />
      </Field>
      <p className="text-right text-xs text-muted-foreground tabular-nums">
        {trimmed.length.toLocaleString()} / {TEACH_MAX.toLocaleString()}
      </p>
      {error && <ErrorNote error={error} />}
    </Dialog>
  );
}
