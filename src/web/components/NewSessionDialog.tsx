import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import { KIND_HINTS, KIND_LABELS, KIND_STAGES, SESSION_KINDS, STAGE_LABELS } from '../../shared/types';
import type { ProjectWithSessions, Session, SessionKind } from '../../shared/types';
import { api, errorMessage } from '../api';
import { cx, slugify } from '../util';
import { Dialog, ErrorText, Field } from './common';

const NAME_PLACEHOLDER: Record<SessionKind, string> = {
  explore: 'why is CI slow on macOS',
  plan: 'fix retry backoff',
  quick: 'fix typo in README',
};

/** First and last stage, e.g. "Design → Merge & Close", or the only one. */
function stageSpan(kind: SessionKind): string {
  const st = KIND_STAGES[kind];
  return st.length === 1 ? STAGE_LABELS[st[0]] : `${STAGE_LABELS[st[0]]} → ${STAGE_LABELS[st[st.length - 1]]}`;
}

/** The session types as boxes side by side, a radio group (arrow keys move the choice). */
function KindPicker({ value, onChange }: { value: SessionKind; onChange: (k: SessionKind) => void }) {
  const refs = useRef<Partial<Record<SessionKind, HTMLButtonElement | null>>>({});
  const onKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const step = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const next = SESSION_KINDS[(SESSION_KINDS.indexOf(value) + step + SESSION_KINDS.length) % SESSION_KINDS.length];
    onChange(next);
    refs.current[next]?.focus();
  };
  return (
    <div className="field">
      <span className="label" id="ns-kind-label">
        Session type
      </span>
      <div className="kinds" role="radiogroup" aria-labelledby="ns-kind-label" aria-describedby="ns-kind-hint" onKeyDown={onKey}>
        {SESSION_KINDS.map((k) => (
          <button
            key={k}
            id={`ns-kind-${k}`}
            ref={(el) => {
              refs.current[k] = el;
            }}
            type="button"
            role="radio"
            aria-checked={k === value}
            tabIndex={k === value ? 0 : -1}
            className={cx('kind', k === value && 'on')}
            onClick={() => onChange(k)}
          >
            <b>{KIND_LABELS[k]}</b>
            <span className="kind-stages">{stageSpan(k)}</span>
          </button>
        ))}
      </div>
      <span className="hint" id="ns-kind-hint">
        {KIND_HINTS[value]}
      </span>
    </div>
  );
}

export function NewSessionDialog({
  project,
  onClose,
  onCreated,
}: {
  project: ProjectWithSessions;
  onClose: () => void;
  onCreated: (s: Session) => void;
}) {
  const [name, setName] = useState('');
  const [prompt, setPrompt] = useState('');
  const [branches, setBranches] = useState<string[] | null>(null);
  const [baseBranch, setBaseBranch] = useState(project.defaultBranch);
  const [kind, setKind] = useState<SessionKind>('plan');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api
      .branches(project.id)
      .then((b) => setBranches(b.includes(project.defaultBranch) ? b : [project.defaultBranch, ...b]))
      .catch(() => setBranches([project.defaultBranch]));
  }, [project.id, project.defaultBranch]);

  const branchPreview = `${project.settings.branchPrefix}${slugify(name) || 'task-name'}`;

  const create = async () => {
    setError(null);
    setBusy(true);
    try {
      const s = await api.createSession(project.id, {
        name: name.trim(),
        prompt: prompt.trim() || undefined,
        baseBranch,
        kind,
      });
      onCreated(s);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={`New session in ${project.name}`}
      onClose={onClose}
      footer={
        <>
          <span className="muted small mono">branch {branchPreview}</span>
          <button className="btn primary" disabled={!name.trim() || busy} onClick={create}>
            {busy ? 'Starting…' : 'Start session'}
          </button>
        </>
      }
    >
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (name.trim() && !busy) create();
        }}
      >
        <Field label="Name">
          <input
            id="ns-name"
            className="input"
            placeholder={NAME_PLACEHOLDER[kind]}
            value={name}
            onChange={(e) => setName(e.target.value)}
            autoFocus
          />
        </Field>
        <KindPicker value={kind} onChange={setKind} />
        <Field label="First prompt (optional)" hint="What should AgentK do? You can also type it in the terminal.">
          <textarea
            id="ns-prompt"
            className="input"
            rows={5}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && name.trim() && !busy) {
                e.preventDefault();
                create();
              }
            }}
          />
        </Field>
        <Field label="Base branch">
          <select id="ns-base" className="input" value={baseBranch} onChange={(e) => setBaseBranch(e.target.value)}>
            {(branches ?? [project.defaultBranch]).map((b) => (
              <option key={b} value={b}>
                {b}
              </option>
            ))}
          </select>
        </Field>
        <ErrorText error={error} />
      </form>
    </Dialog>
  );
}
