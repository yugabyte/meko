import { useEffect, useState } from 'react';
import type { DatapackRef, MekoRef } from '../../shared/types';
import { copyText } from '../clipboard';

/** Meko's "m" mark (`public/meko-m.png`), drawn in the text color around it. */
export function MekoMark() {
  return <span className="meko-mark" aria-hidden="true" />;
}

/** The badge on a design decision (or a question's recommendation) that rests on Meko. */
export function MekoBadge() {
  return (
    <span className="dq-chip meko" role="img" aria-label="From Meko" title="From Meko">
      <MekoMark />
    </span>
  );
}

/**
 * The Meko items a design decision rests on, as Kode cited them: the words it relied on, and
 * the item's kind, datapack and id, with Copy id to find it in Meko later. Nothing is read from
 * Meko, so they show with Meko off too. All of it is plain text: shared datapacks are written by
 * other people.
 */
export function MekoRefs({ refs, datapacks = [] }: { refs: MekoRef[]; datapacks?: DatapackRef[] }) {
  // The id last copied, so "Copied" shows only on its button, and only for a moment.
  const [copied, setCopied] = useState<string | null>(null);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(null), 2500);
    return () => clearTimeout(t);
  }, [copied]);
  if (!refs.length) return null;
  return (
    <ul className="dq-refs" aria-label="From Meko">
      {refs.map((r) => {
        const dp = r.datapackId ? (datapacks.find((d) => d.id === r.datapackId)?.name ?? r.datapackId) : null;
        return (
          <li key={`${r.kind}:${r.id}`} className="dq-ref">
            {r.excerpt && <p className="dq-ref-text">“{r.excerpt}”</p>}
            <div className="dq-ref-src">
              <MekoMark />
              <span>{r.kind === 'memory' ? 'Memory' : 'Knowledge base'}</span>
              {dp && <span className="dq-ref-dp">{dp}</span>}
              <code className="dq-ref-id">{r.id}</code>
              <button type="button" className="btn small" onClick={() => void copyText(r.id).then((ok) => setCopied(ok ? r.id : null))}>
                {copied === r.id ? 'Copied' : 'Copy id'}
              </button>
            </div>
          </li>
        );
      })}
    </ul>
  );
}
