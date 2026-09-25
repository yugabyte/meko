// Copying terminal text to the browser clipboard.

/**
 * The text of an OSC 52 "set clipboard" sequence (`<targets>;<base64>`), which tmux sends
 * when you select with the mouse. Null for queries (`?`), clears and malformed data.
 */
export function decodeOsc52(data: string): string | null {
  const i = data.indexOf(';');
  if (i < 0) return null;
  const b64 = data.slice(i + 1);
  if (!b64 || b64 === '?') return null;
  try {
    const bin = atob(b64);
    return new TextDecoder().decode(Uint8Array.from(bin, (c) => c.charCodeAt(0)));
  } catch {
    return null;
  }
}

/** Puts text on the clipboard. The async Clipboard API needs HTTPS (or localhost); the fallback needs a user gesture. */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    /* fall back below */
  }
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  const active = document.activeElement as HTMLElement | null;
  document.body.appendChild(ta);
  ta.select();
  let ok: boolean;
  try {
    ok = document.execCommand('copy');
  } catch {
    ok = false;
  }
  ta.remove();
  active?.focus();
  return ok;
}
