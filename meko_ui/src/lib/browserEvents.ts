type AppEventDetail = Record<string, unknown> | undefined;

export function dispatchAppEvent(name: string, detail?: AppEventDetail) {
  globalThis.dispatchEvent(new CustomEvent(name, { detail }));
}

export function addAppEventListener(
  name: string,
  listener: EventListenerOrEventListenerObject,
) {
  globalThis.addEventListener(name, listener);
}

export function removeAppEventListener(
  name: string,
  listener: EventListenerOrEventListenerObject,
) {
  globalThis.removeEventListener(name, listener);
}
