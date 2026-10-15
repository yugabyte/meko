import { useState } from 'react';
import { MAX_KB_UPLOAD_BYTES } from '../../shared/types';
import { api, errorMessage } from '../api';
import { fileSize } from '../knowledge';
import { Busy, Dialog } from './common';

/** Where one file is: not sent yet, sending, in Meko (indexing, or it was already there), or refused. */
type FileState = { at: 'waiting' } | { at: 'sending' } | { at: 'added' } | { at: 'already' } | { at: 'failed'; error: string };

const LIMIT_MB = MAX_KB_UPLOAD_BYTES / 1024 / 1024;

/**
 * Asks before adding files dropped on (or picked for) a session's terminal to its project datapack's
 * knowledge base, then uploads them one at a time with each one's result. `onDone` gets the
 * summary for the terminal notice once every file went through; a refusal keeps the dialog open.
 */
export function KnowledgeUploadDialog({
  sessionId,
  datapackName,
  files,
  onClose,
}: {
  sessionId: string;
  datapackName: string;
  files: File[];
  onClose: (summary: string | null) => void;
}) {
  const fits = files.filter((f) => f.size > 0 && f.size <= MAX_KB_UPLOAD_BYTES);
  const skipped = files.filter((f) => !fits.includes(f));
  const [states, setStates] = useState<FileState[]>(() => fits.map(() => ({ at: 'waiting' })));
  const [busy, setBusy] = useState(false);
  const sent = states.some((s) => s.at !== 'waiting');

  const upload = async () => {
    if (busy || sent || !fits.length) return;
    setBusy(true);
    const done: FileState[] = [...states];
    for (let i = 0; i < fits.length; i++) {
      done[i] = { at: 'sending' };
      setStates([...done]);
      try {
        const r = await api.uploadKnowledge(sessionId, fits[i]);
        done[i] = { at: r.status === 'processing' ? 'added' : 'already' };
      } catch (e) {
        done[i] = { at: 'failed', error: errorMessage(e) };
      }
      setStates([...done]);
    }
    setBusy(false);
    if (!done.some((s) => s.at === 'failed')) onClose(summary(fits, done));
  };

  const close = () => {
    if (busy) return;
    onClose(sent ? summary(fits, states) : null);
  };

  return (
    <Dialog
      title="Add to the knowledge base?"
      onClose={close}
      footer={
        sent && !busy ? (
          <button className="btn primary" onClick={close}>
            Close
          </button>
        ) : (
          <>
            <button className="btn" disabled={busy} onClick={close}>
              Cancel
            </button>
            <button className="btn primary" disabled={busy || !fits.length} onClick={() => void upload()}>
              {busy ? <Busy label="Uploading" /> : 'Upload'}
            </button>
          </>
        )
      }
    >
      <div className="stack">
        <p>
          Uploads to the datapack <span className="mono">{datapackName}</span>. Everyone with access to the datapack can find
          what's in {fits.length === 1 ? 'this file' : 'these files'}.
        </p>
        <ul className="kb-files">
          {fits.map((f, i) => (
            <li key={`${f.name}-${i}`}>
              <span className="kb-name">{f.name}</span>
              <span className="muted small">{fileSize(f.size)}</span>
              <FileResult state={states[i]} />
            </li>
          ))}
          {skipped.map((f, i) => (
            <li key={`skip-${f.name}-${i}`} className="skipped">
              <span className="kb-name">{f.name}</span>
              <span className="muted small">{f.size ? `Skipped: over ${LIMIT_MB} MB` : 'Skipped: empty'}</span>
            </li>
          ))}
        </ul>
      </div>
    </Dialog>
  );
}

function FileResult({ state }: { state: FileState }) {
  if (state.at === 'sending') return <span className="small"><Busy label="Uploading" /></span>;
  if (state.at === 'added') return <span className="ok-text small">Indexing</span>;
  if (state.at === 'already') return <span className="ok-text small">Already in the knowledge base</span>;
  if (state.at === 'failed') return <span className="error-text kb-error">{state.error}</span>;
  return null;
}

/** The terminal notice once the uploads are over: what was added, what was there, what failed. */
function summary(files: File[], states: FileState[]): string | null {
  const count = (at: FileState['at']) => states.filter((s) => s.at === at).length;
  const nFiles = (n: number) => (n === 1 ? '1 file' : `${n} files`);
  const added = count('added');
  const already = count('already');
  const failed = count('failed');
  const parts: string[] = [];
  if (added) parts.push(`${added === 1 ? files[states.findIndex((s) => s.at === 'added')].name : nFiles(added)} added to the knowledge base. Meko is indexing ${added === 1 ? 'it' : 'them'}.`);
  if (already) parts.push(`${nFiles(already)} ${already === 1 ? 'was' : 'were'} already there.`);
  if (failed) parts.push(`${nFiles(failed)} couldn’t be added.`);
  return parts.length ? parts.join(' ') : null;
}
