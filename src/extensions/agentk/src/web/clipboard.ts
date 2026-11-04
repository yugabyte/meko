// Copying terminal text to the browser clipboard.

/**
 * The text of an OSC 52 "set clipboard" sequence (`<targets>;<base64>`), which a program in the
 * terminal sends to copy text (Claude Code does, for a selection it makes itself). Null for queries (`?`), clears and malformed data.
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

/** What `pastedImages` needs of a paste's or drop's DataTransfer. */
export interface Transfer<F> {
  files?: ArrayLike<F> | null;
  getData?(format: string): string;
}

/**
 * The images in a paste or drop. Text alongside them makes it a text paste (a spreadsheet's cells
 * come with a picture of them), unless the text is only the files' names, as when copying files.
 */
export function pastedImages<F extends { type: string; name?: string }>(dt: Transfer<F> | null | undefined): F[] {
  return onlyNamed(Array.from(dt?.files ?? []).filter((f) => f.type.startsWith('image/')), dt);
}

/** The files of any type in a paste or drop, by the same rule as `pastedImages` for text alongside them. */
export function pastedFiles<F extends { type: string; name?: string }>(dt: Transfer<F> | null | undefined): F[] {
  return onlyNamed(Array.from(dt?.files ?? []), dt);
}

/** `files`, unless the transfer's text is more than their names. */
function onlyNamed<F extends { name?: string }>(files: F[], dt: Transfer<F> | null | undefined): F[] {
  if (!files.length) return [];
  let text = '';
  try {
    text = dt?.getData?.('text/plain') ?? '';
  } catch {
    /* no text to read */
  }
  const names = new Set(files.map((f) => f.name));
  const lines = text.split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
  return lines.every((l) => names.has(l)) ? files : [];
}

/**
 * The images on the clipboard, read with the async Clipboard API (for a key press that fires no
 * paste event). Empty when there's text too or no image; null when the browser can't or won't
 * let the page read it.
 */
export async function readClipboardImages(): Promise<Blob[] | null> {
  if (!navigator.clipboard?.read) return null;
  let items: ClipboardItems;
  try {
    items = await navigator.clipboard.read();
  } catch {
    return null;
  }
  if (items.some((i) => i.types.includes('text/plain'))) return [];
  const out: Blob[] = [];
  for (const item of items) {
    const type = item.types.find((t) => t.startsWith('image/'));
    if (!type) continue;
    try {
      out.push(await item.getType(type));
    } catch {
      /* gone from the clipboard meanwhile */
    }
  }
  return out;
}
