// Key/value rows for one settings surface.
//
// A settings object is a flat map, so it edits as rows — you can see what is
// set without reading JSON, and adding one setting does not mean getting the
// punctuation of a whole document right.
//
// The wrinkle a grid has to solve is that values are typed. `pg_settings` are
// always strings; a lease is numbers; a daemon override may be a string, an int
// or a bool (`tenant_config.enable_gc` is a bool, `pitr_interval` is "7 days").
// Text inputs give us strings, so `valueOf` decides what a row means — and for
// scalar surfaces an existing row keeps the type it was stored with, so editing
// a value that merely looks numeric cannot silently retype it.

import { Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { dataBranchingMessages as msg } from "@/lib/messages";

/** How a surface's values are typed on the wire. */
export type ValueKind = "string" | "number" | "scalar";

/** One editable setting. `storedKind` is the type this row's value had when it
 *  was read, so an edit preserves it rather than re-inferring. */
export type Row = {
  key: string;
  value: string;
  storedKind?: "string" | "number" | "boolean";
};

/** Rows for an object read from the API, in a stable order so the list does not
 *  reshuffle between openings. */
export function rowsOf(obj: unknown): Row[] {
  if (!obj || typeof obj !== "object") return [];
  return Object.entries(obj as Record<string, unknown>)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([key, v]) => ({
      key,
      value: String(v),
      storedKind:
        typeof v === "number" ? "number" : typeof v === "boolean" ? "boolean" : "string",
    }));
}

/** The value a row means, or `undefined` when it cannot mean anything. */
export function valueOf(row: Row, kind: ValueKind): string | number | boolean | undefined {
  const text = row.value.trim();
  if (kind === "string") return row.value;
  if (kind === "number") {
    const n = Number(text);
    return text !== "" && Number.isFinite(n) ? n : undefined;
  }
  // scalar: keep what it was stored as, so "123" that was a string stays one.
  if (row.storedKind === "string") return row.value;
  if (row.storedKind === "number" || row.storedKind === "boolean" || !row.storedKind) {
    if (text === "true") return true;
    if (text === "false") return false;
    const n = Number(text);
    if (text !== "" && Number.isFinite(n)) return n;
  }
  return row.value;
}

/** Why a set of rows cannot be turned into settings. */
export type RowsError =
  | { kind: "blank"; key: string }
  | { kind: "duplicate"; key: string }
  | { kind: "type"; key: string };

/** Rows → the object to send, or why they cannot be.
 *
 * Refusing beats guessing on every branch here, because the save that carries
 * these values also restarts the service that reads them: a wrong value is not
 * a bad row in a table, it is a daemon that will not come back up.
 */
export function objectOf(
  rows: Row[],
  kind: ValueKind,
): { ok: true; value: Record<string, string | number | boolean> } | { ok: false; error: RowsError } {
  const out: Record<string, string | number | boolean> = {};
  for (const row of rows) {
    const key = row.key.trim();
    if (!key) continue; // a row the operator has not filled in yet
    // A named setting with no value is almost always an unfinished row, and
    // sending it is expensive: `work_mem = ''` renders into postgresql.conf and
    // Postgres refuses to start on the restart this same save triggers. Nothing
    // downstream validates pg_settings values, so this is the only place that
    // can catch it.
    if (row.value.trim() === "") return { ok: false, error: { kind: "blank", key } };
    // Two rows with one name would silently collapse, last-wins, and the list
    // the operator is looking at would not be what got sent.
    if (Object.prototype.hasOwnProperty.call(out, key)) {
      return { ok: false, error: { kind: "duplicate", key } };
    }
    const value = valueOf(row, kind);
    if (value === undefined) return { ok: false, error: { kind: "type", key } };
    out[key] = value;
  }
  return { ok: true, value: out };
}

export function SettingsRows({
  id,
  rows,
  kind,
  onChange,
}: {
  id: string;
  rows: Row[];
  kind: ValueKind;
  onChange: (rows: Row[]) => void;
}) {
  const set = (i: number, patch: Partial<Row>) =>
    onChange(rows.map((r, n) => (n === i ? { ...r, ...patch } : r)));

  return (
    // A labelled group rather than a label pointing at one input: the surface
    // has many controls, so there is no single element for `htmlFor` to name.
    <div className="space-y-2" role="group" aria-labelledby={`${id}-label`}>
      {rows.length === 0 && (
        <p className="text-muted-foreground text-sm">{msg.settingsNothingSet}</p>
      )}
      {rows.map((row, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            aria-label={`${id} name ${i + 1}`}
            className="font-mono text-sm"
            value={row.key}
            onChange={(e) => set(i, { key: e.target.value })}
          />
          <Input
            aria-label={`${id} value ${i + 1}`}
            className="font-mono text-sm"
            inputMode={kind === "number" ? "numeric" : undefined}
            value={row.value}
            onChange={(e) => set(i, { value: e.target.value })}
          />
          <Button
            type="button"
            variant="ghost"
            size="sm"
            aria-label={`${id} remove ${i + 1}`}
            onClick={() => onChange(rows.filter((_, n) => n !== i))}
            className="h-8 px-2"
          >
            <Trash2 className="h-3.5 w-3.5" />
          </Button>
        </div>
      ))}
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => onChange([...rows, { key: "", value: "" }])}
        className="h-7 text-xs"
      >
        {msg.settingsAddRow}
      </Button>
    </div>
  );
}
