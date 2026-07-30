/**
 * Uploads a single file to a presigned S3/MinIO URL via a direct browser PUT.
 *
 * The browser PUTs straight to the presigned (MinIO/S3) URL, bypassing the
 * api_server. Over a forwarded/tunneled endpoint this can be far slower than
 * the server's own loopback, so we use a *stall-based* timeout — the upload is
 * aborted only if no bytes move for `stallMs` — rather than a fixed total
 * duration cap that would kill large-but-progressing uploads mid-transfer.
 *
 * @param url        Presigned PUT URL.
 * @param file       File to upload; its `type` is sent as Content-Type.
 * @param onProgress Optional 0–100 percentage callback (only fires when the
 *                   browser reports a computable length).
 * @param stallMs    No-progress window before aborting. Defaults to 45s.
 */
const UPLOAD_RESPONSE_TIMEOUT_MS = 30_000;

export function uploadFileToS3(
  url: string,
  file: File,
  onProgress?: (pct: number) => void,
  stallMs = 45_000,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    let stallTimer: ReturnType<typeof setTimeout>;
    const armStall = () => {
      clearTimeout(stallTimer);
      stallTimer = setTimeout(() => {
        reject(new Error(`Upload stalled (no progress for ${stallMs / 1000}s)`));
        xhr.abort();
      }, stallMs);
    };
    armStall();
    xhr.open("PUT", url);
    xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");

    xhr.upload.onprogress = (e) => {
      armStall();
      if (e.lengthComputable) {
        onProgress?.(Math.round((e.loaded / e.total) * 100));
      }
    };

    // Upload progress stops once the body is sent; switch to a fixed window
    // for the server response so a hung gateway cannot leave the promise pending.
    xhr.upload.onload = () => {
      clearTimeout(stallTimer);
      stallTimer = setTimeout(() => {
        reject(new Error(`Upload response timed out (no response for ${UPLOAD_RESPONSE_TIMEOUT_MS / 1000}s)`));
        xhr.abort();
      }, UPLOAD_RESPONSE_TIMEOUT_MS);
    };

    xhr.onload = () => {
      clearTimeout(stallTimer);
      if (xhr.status >= 200 && xhr.status < 300) resolve();
      else reject(new Error(`${xhr.status} ${xhr.statusText}: ${xhr.responseText}`));
    };
    xhr.onerror = () => {
      clearTimeout(stallTimer);
      reject(new Error(`Network error (status=${xhr.status})`));
    };
    xhr.onabort = () => {
      clearTimeout(stallTimer);
      reject(new Error("Upload aborted"));
    };
    xhr.send(file);
  });
}
