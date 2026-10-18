import { useEffect, useState, type ReactNode } from 'react';
import { copyText } from '../clipboard';
import { httpHost, mekoKeyLinks } from '../util';

/**
 * Where to get a Meko account and API key: Sign up and Log in links to the Meko web app, the steps
 * to make a key, and its Settings page written out (with Copy) for a browser on another device.
 * `fallback` replaces the links when the web app isn't known.
 */
export function MekoGetKey({ title, webUrl, fallback }: { title: string; webUrl: string | null; fallback: ReactNode }) {
  // The URL last copied, so "Copied" shows only for that URL, and only for a moment.
  const [copied, setCopied] = useState<string | null>(null);
  const links = mekoKeyLinks(webUrl);

  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(null), 2500);
    return () => clearTimeout(t);
  }, [copied]);

  return (
    <div className="meko-getkey" role="note">
      <b>{title}</b>
      {links ? (
        <>
          <div className="meko-getkey-links">
            <a className="btn small primary" href={links.signup} target="_blank" rel="noreferrer">
              Sign up ↗
            </a>
            <a className="btn small" href={links.signin} target="_blank" rel="noreferrer">
              Log in ↗
            </a>
            <span className="muted">at {httpHost(links.settings)}</span>
          </div>
          <p>
            Then open Settings → Security → <i>Generate API key</i>, and paste the key below. On another device? Open:
          </p>
          <div className="meko-getkey-url">
            <code>{links.settings}</code>
            <button type="button" className="btn small" onClick={() => void copyText(links.settings).then((ok) => setCopied(ok ? links.settings : null))}>
              {copied === links.settings ? 'Copied' : 'Copy'}
            </button>
          </div>
        </>
      ) : (
        <p>{fallback}</p>
      )}
    </div>
  );
}
