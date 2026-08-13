// One-shot "you arrived here, look at this" scroll + flash.
//
// Deep links into the Knowledge Base (a row clicked in the preview tile
// or the upload dialog, a promoted memory opened from the trace drawer)
// land on a page that may be paginated, tabbed, and still fetching. The
// target element therefore mounts some indeterminate time after the
// navigation, and the "flash it once" intent has to survive that gap
// without ever firing twice.
//
// Doing this inline in each component kept breaking the same way: the
// effect scheduled a timer, then a *cleanup* cancelled it before it ran.
// Cleanups fire whenever the dep array changes, and the natural deps for
// these effects change constantly -- a parent's inline `onDone` arrow is
// a new function every render, a filtered list is a new array every
// render, and `active` itself flips false as soon as the arrival is
// marked consumed. The one-shot guard ref had already been set by then,
// so the retry never came and nothing was ever visible.
//
// So: timers live in a ref and are only cleared on unmount, never on a
// dep change. `onDone` is read through a ref for the same reason. The
// per-key guard means the same target can't flash twice while this
// component stays mounted (paging away and back, a background refetch
// remounting the row, a tab switch), which is the "only once, ever"
// requirement.
import { useCallback, useEffect, useRef, useState } from "react";

const FLASH_DURATION_MS = 1600;

type UseArrivalFlashArgs = {
  /** The id being flashed. `undefined` (or `active: false`) means no
   *  pending arrival -- nothing is scheduled. */
  flashKey?: string;
  /** Gate for "the target is actually rendered right now". Callers pass
   *  the result of looking the key up in their currently-visible rows;
   *  scheduling before that would scroll to a ref that's still null. */
  active: boolean;
  /** Fired once the flash has finished, so the caller can retire the
   *  arrival intent. Read through a ref -- an inline arrow is fine. */
  onDone?: (key: string) => void;
  /** Passed through to scrollIntoView. */
  block?: ScrollLogicalPosition;
  /** Prefix for the debug trace, e.g. "FileRow". */
  debugLabel: string;
};

export function useArrivalFlash<T extends HTMLElement>({
  flashKey,
  active,
  onDone,
  block = "center",
  debugLabel,
}: UseArrivalFlashArgs) {
  const ref = useRef<T>(null);
  const [flashing, setFlashing] = useState(false);

  const onDoneRef = useRef(onDone);
  onDoneRef.current = onDone;

  const flashedKeysRef = useRef<Set<string>>(new Set());
  const timersRef = useRef<number[]>([]);
  const framesRef = useRef<number[]>([]);

  const log = useCallback(
    (event: string, extra?: Record<string, unknown>) => {
      console.log(
        `[arrival-flash] ${debugLabel} ${event}`,
        JSON.stringify(
          {
            flashKey: flashKey ?? null,
            active,
            alreadyFlashed: flashKey ? flashedKeysRef.current.has(flashKey) : false,
            hasNode: Boolean(ref.current),
            ...extra,
          },
          null,
          2,
        ),
      );
    },
    [debugLabel, flashKey, active],
  );

  useEffect(() => {
    if (!flashKey || !active) return;
    if (flashedKeysRef.current.has(flashKey)) {
      log("skip: already flashed this key");
      return;
    }
    flashedKeysRef.current.add(flashKey);
    log("scheduling");

    // Two frames, not a timeout: the first lands after React has
    // committed this render, the second after the browser has laid it
    // out -- so the node exists and has a real box before we ask to
    // scroll to it. A fixed delay would either be too short on a slow
    // render or needlessly laggy on a fast one.
    const frame1 = window.requestAnimationFrame(() => {
      const frame2 = window.requestAnimationFrame(() => {
        const node = ref.current;
        log("firing", { nodeFound: Boolean(node) });
        node?.scrollIntoView({ behavior: "smooth", block });
        setFlashing(true);

        const done = window.setTimeout(() => {
          setFlashing(false);
          log("done");
          onDoneRef.current?.(flashKey);
        }, FLASH_DURATION_MS);
        timersRef.current.push(done);
      });
      framesRef.current.push(frame2);
    });
    framesRef.current.push(frame1);
    // Deliberately no cleanup: see the file header. Pending work is torn
    // down on unmount by the effect below, never on a dep change.
  }, [flashKey, active, block, log]);

  useEffect(
    () => () => {
      timersRef.current.forEach(window.clearTimeout);
      framesRef.current.forEach(window.cancelAnimationFrame);
    },
    [],
  );

  return { ref, flashing };
}
