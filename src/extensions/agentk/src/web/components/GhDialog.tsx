import { useEffect, useState, type ReactNode } from 'react';
import type { GhStatus } from '../../shared/types';
import { api, errorMessage } from '../api';
import { copyText } from '../clipboard';
import { Busy, Dialog, ErrorText } from './common';

const GH_SITE = 'https://cli.github.com';

/**
 * The GitHub dialog: an action that works on GitHub was refused because gh isn't installed or
 * signed in (`reason`, the refusal's words). It installs gh (no root) and signs it in with GitHub's
 * one-time code, from any browser, a phone's included; then the user tries the action again.
 */
export function GhDialog({ reason, gh, onClose }: { reason: string; gh: GhStatus; onClose: () => void }) {
  const [error, setError] = useState<string | null>(null);
  const [asking, setAsking] = useState(false);
  const [copied, setCopied] = useState(false);
  const job = gh.job;

  // What the server last found may be old: gh installed or signed in since.
  useEffect(() => {
    void api.ghCheck().catch(() => undefined);
  }, []);

  const call = async (fn: () => Promise<GhStatus>) => {
    setError(null);
    setAsking(true);
    try {
      await fn();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setAsking(false);
    }
  };

  const close = () => {
    // A sign-in under way stops, and a failed install or sign-in is forgotten.
    if (job && (job.phase === 'failed' || job.kind === 'login')) void api.ghForget().catch(() => undefined);
    onClose();
  };

  const checkAgain = (
    <button className="btn" disabled={asking} onClick={() => void call(api.ghCheck)}>
      Check again
    </button>
  );

  let body: ReactNode;
  let footer: ReactNode;
  if (job?.phase === 'running') {
    body = <p><Busy label={job.kind === 'install' ? 'Installing gh' : 'Starting the sign-in'} /></p>;
  } else if (job?.kind === 'login' && job.phase === 'code' && job.code) {
    const url = job.url ?? 'https://github.com/login/device';
    body = (
      <>
        <p>Enter this code on GitHub to sign gh in:</p>
        <p className="gh-code" id="gh-code">{job.code}</p>
        <p className="muted small">This goes on by itself once GitHub has the code.</p>
      </>
    );
    footer = (
      <>
        <button className="btn" onClick={() => void copyText(job.code ?? '').then(setCopied)}>
          {copied ? 'Copied' : 'Copy code'}
        </button>
        <a className="btn primary" href={url} target="_blank" rel="noopener noreferrer">
          Open GitHub
        </a>
      </>
    );
  } else if (job?.phase === 'failed') {
    body = (
      <>
        <p>{job.kind === 'install' ? "gh couldn't be installed." : "gh couldn't be signed in."}</p>
        {job.message && <p className="muted small">{job.message}</p>}
        {job.kind === 'install' && (
          <p className="muted small">
            Install it yourself from <a href={GH_SITE} target="_blank" rel="noopener noreferrer">cli.github.com</a>, then check again.
          </p>
        )}
      </>
    );
    footer = (
      <>
        {checkAgain}
        <button className="btn primary" disabled={asking} onClick={() => void call(job.kind === 'install' ? api.ghInstall : api.ghLogin)}>
          Try again
        </button>
      </>
    );
  } else if (gh.state === 'ok') {
    body = <p>gh is signed in to GitHub{gh.login ? ` as ${gh.login}` : ''}. Try again.</p>;
    footer = (
      <button className="btn primary" onClick={close}>
        Done
      </button>
    );
  } else if (gh.state === 'signed-out') {
    body = (
      <>
        <p>{reason}</p>
        <p className="muted small">Kode shows a one-time code to enter on GitHub, from any device.</p>
      </>
    );
    footer = (
      <>
        {checkAgain}
        <button className="btn primary" disabled={asking} onClick={() => void call(api.ghLogin)}>
          Sign in to GitHub
        </button>
      </>
    );
  } else {
    body = (
      <>
        <p>{reason}</p>
        <p className="muted small">Kode installs it for you, without root.</p>
      </>
    );
    footer = (
      <>
        {checkAgain}
        <button className="btn primary" disabled={asking || gh.state === 'unknown'} onClick={() => void call(api.ghInstall)}>
          Install gh
        </button>
      </>
    );
  }

  return (
    <Dialog title="GitHub" onClose={close} footer={footer}>
      <div className="stack">
        {body}
        <ErrorText error={error} />
      </div>
    </Dialog>
  );
}
