// Saves bytes the API returned inline (base64) as a file in the browser's downloads.

export function saveBase64(filename: string, contentType: string, base64: string): void {
  const bin = atob(base64);
  const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
  const url = URL.createObjectURL(new Blob([bytes], { type: contentType || "application/octet-stream" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  // Revoking at once can cancel the download in some browsers.
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
