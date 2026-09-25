import { useEffect, useState } from 'react';
import type { ProjectWithSessions, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { slugify } from '../util';
import { Dialog, ErrorText, Field } from './common';

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
  const [skipDesign, setSkipDesign] = useState(false);
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
        skipDesign,
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
            placeholder="fix retry backoff"
            value={name}
            onChange={(e) => setName(e.target.value)}
            autoFocus
          />
        </Field>
        <Field label="First prompt (optional)" hint="What should Claude work on? You can also type it in the terminal.">
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
        <label className="check">
          <input id="ns-skip" type="checkbox" checked={skipDesign} onChange={(e) => setSkipDesign(e.target.checked)} />
          <span>
            Skip design (small task)
            <span className="hint block">Claude starts coding straight away instead of writing a design first.</span>
          </span>
        </label>
        <ErrorText error={error} />
      </form>
    </Dialog>
  );
}
