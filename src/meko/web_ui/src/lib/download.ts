// Saves bytes the API returned as a file in the browser's downloads: inline (base64) or as a blob.

export function saveBase64(filename: string, contentType: string, base64: string): void {
  const bin = atob(base64);
  const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
  saveBlob(filename, new Blob([bytes], { type: contentType || "application/octet-stream" }));
}

export function saveBlob(filename: string, blob: Blob): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  // Revoking at once can cancel the download in some browsers.
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
