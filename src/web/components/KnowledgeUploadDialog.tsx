import { MAX_KB_UPLOAD_BYTES } from '../../shared/types';
import { fileSize, type KnowledgeBatch, type KnowledgeFileState } from '../knowledge';
import { Busy, Dialog } from './common';

const LIMIT_MB = MAX_KB_UPLOAD_BYTES / 1024 / 1024;

/**
 * Asks before adding files dropped on (or picked for) a session's terminal to its project datapack's
 * knowledge base, and shows each one's upload. The batch, and the uploads, live on the terminal
 * (`uploadKnowledge()` in `terminals.ts`), so switching sessions mid-upload and coming back shows
 * where they are. `onClose` is Cancel, or Close once the uploads are over; it does nothing while one runs.
 */
export function KnowledgeUploadDialog({
  datapackName,
  batch,
  onUpload,
  onClose,
}: {
  datapackName: string;
  batch: KnowledgeBatch;
  onUpload: () => void;
  onClose: () => void;
}) {
  const { files, states, skipped, busy } = batch;
  const sent = states.some((s) => s.at !== 'waiting');

  return (
    <Dialog
      title="Add to the knowledge base?"
      onClose={onClose}
      footer={
        sent && !busy ? (
          <button className="btn primary" onClick={onClose}>
            Close
          </button>
        ) : (
          <>
            <button className="btn" disabled={busy} onClick={onClose}>
              Cancel
            </button>
            <button className="btn primary" disabled={busy || !files.length} onClick={onUpload}>
              {busy ? <Busy label="Uploading" /> : 'Upload'}
            </button>
          </>
        )
      }
    >
      <div className="stack">
        <p>
          Uploads to the datapack <span className="mono">{datapackName}</span>. Everyone with access to the datapack can find
          what's in {files.length === 1 ? 'this file' : 'these files'}.
        </p>
        <ul className="kb-files">
          {files.map((f, i) => (
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

function FileResult({ state }: { state: KnowledgeFileState }) {
  if (state.at === 'sending') return <span className="small"><Busy label="Uploading" /></span>;
  if (state.at === 'indexing') return <span className="ok-text small">Indexing</span>;
  if (state.at === 'added') return <span className="ok-text small">Added</span>;
  if (state.at === 'already') return <span className="ok-text small">Already in the knowledge base</span>;
  if (state.at === 'failed') return <span className="error-text kb-error">{state.error}</span>;
  return null;
}
