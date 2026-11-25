// Images (and, on a terminal, files of any type) waiting to go to Claude with the next Enter or
// Send. Each one uploads as soon as it's added, so sending only pastes the saved paths. No DOM here
// (the terminal passes how to upload, name and preview one), so it's unit tested.

export interface HeldImage {
  id: number;
  /** A URL that previews the image (an object URL in the page); null for a file that isn't one. */
  thumb: string | null;
  /** The file's own name ('' for a pasted image, which has none). */
  name: string;
  /** Where the host saved it, or null while it uploads. */
  path: string | null;
}

export interface HeldImagesDeps<B> {
  /** Uploads an image and resolves to the path the host saved it at. */
  upload(image: B): Promise<string>;
  /** A preview URL, or null for a file that isn't an image. */
  thumb(image: B): string | null;
  /** Its own name, if it has one. */
  name?(image: B): string;
  revoke(url: string): void;
  /** The list changed: an image was added, finished uploading, or was removed. */
  onChange(): void;
  /** An upload failed. Its image is already removed. */
  onError(error: unknown): void;
}

export class HeldImages<B> {
  private items: HeldImage[] = [];
  /**
   * Each image's upload (`done`: whether it succeeded), and `drop`, which settles `done` at once when
   * the image is removed, so ready() never waits on an upload nobody wants (a stalled fetch included).
   */
  private uploads = new Map<number, { done: Promise<boolean>; drop: () => void }>();
  /** What each one held is, for a caller that sends it somewhere else too (Upload to Meko). */
  private sources = new Map<number, B>();
  private nextId = 1;

  constructor(private deps: HeldImagesDeps<B>) {}

  /** The images, in the order they were added. */
  get list(): readonly HeldImage[] {
    return this.items;
  }

  get size(): number {
    return this.items.length;
  }

  /** Whether any image is still uploading. */
  get uploading(): boolean {
    return this.items.some((i) => i.path === null);
  }

  add(images: B[]) {
    if (!images.length) return;
    for (const image of images) {
      const id = this.nextId++;
      this.items = [...this.items, { id, thumb: this.deps.thumb(image), name: this.deps.name?.(image) ?? '', path: null }];
      this.sources.set(id, image);
      const upload = this.deps.upload(image).then(
        (path) => {
          if (!this.has(id)) return true;
          this.items = this.items.map((i) => (i.id === id ? { ...i, path } : i));
          this.deps.onChange();
          return true;
        },
        (e: unknown) => {
          // Removed while it uploaded: nobody is waiting on it.
          if (!this.has(id)) return true;
          // Not through drop(): a waiting ready() has to see this upload fail.
          this.uploads.delete(id);
          this.remove(id);
          this.deps.onError(e);
          return false;
        },
      );
      let drop = () => {};
      const dropped = new Promise<boolean>((r) => (drop = () => r(true)));
      this.uploads.set(id, { done: Promise.race([upload, dropped]), drop });
    }
    this.deps.onChange();
  }

  remove(id: number) {
    const item = this.items.find((i) => i.id === id);
    if (!item) return;
    if (item.thumb) this.deps.revoke(item.thumb);
    this.items = this.items.filter((i) => i.id !== id);
    this.forget(id);
    this.deps.onChange();
  }

  /** Removes these images (the ones just sent). Images added since stay. */
  release(sent: readonly HeldImage[]) {
    const ids = new Set(sent.map((i) => i.id));
    const gone = this.items.filter((i) => ids.has(i.id));
    if (!gone.length) return;
    for (const i of gone) {
      if (i.thumb) this.deps.revoke(i.thumb);
      this.forget(i.id);
    }
    this.items = this.items.filter((i) => !ids.has(i.id));
    this.deps.onChange();
  }

  clear() {
    this.release(this.items);
  }

  /**
   * The images held now, once their uploads are done, each with its path. Null when one of them
   * failed: it's removed and reported (onError), and the rest stay held.
   */
  async ready(): Promise<HeldImage[] | null> {
    const ids = this.items.map((i) => i.id);
    const ok = await Promise.all(ids.map((id) => this.uploads.get(id)?.done ?? true));
    if (ok.includes(false)) return null;
    // An image removed while this waited is left out.
    return this.items.filter((i) => ids.includes(i.id) && i.path !== null);
  }

  /** What a held one is (the blob or file it was added as), while it's held. */
  source(id: number): B | undefined {
    return this.sources.get(id);
  }

  private forget(id: number) {
    this.uploads.get(id)?.drop();
    this.uploads.delete(id);
    this.sources.delete(id);
  }

  private has(id: number): boolean {
    return this.items.some((i) => i.id === id);
  }
}
