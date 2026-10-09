/**
 * Uploads a single file straight to S3/MinIO with one presigned POST
 * request. A presigned POST rather than a PUT so the per-file size cap is
 * enforced by S3 itself, inside the signed policy — a PUT can only pin an
 * exact Content-Length, never a ceiling, leaving the declared size
 * unverified against the bytes actually sent.
 *
 * Goes direct to the bucket, bypassing api_server, over a forwarded/
 * tunneled endpoint that can be far slower than the server's own loopback —
 * so the timeout is *stall-based* (aborted only if no bytes move for
 * `stallMs`) rather than a total-duration cap that would kill a large but
 * healthy transfer mid-flight.
 *
 * @param url        Presigned POST URL (the bucket endpoint).
 * @param fields     Signed form fields from /upload-url, sent verbatim.
 * @param file       File to upload.
 * @param onProgress Optional 0–100 percentage callback, one decimal place.
 * @param stallMs    No-progress window before aborting. Defaults to 45s.
 */
import { verifyFileContent } from "./sniffFileType";
import { safeOrigin } from "./url";

const UPLOAD_RESPONSE_TIMEOUT_MS = 30_000;

export async function uploadFileToS3(
  url: string,
  fields: Record<string, string>,
  file: File,
  onProgress?: (pct: number) => void,
  stallMs = 45_000,
): Promise<void> {
  // Sniff before spending the transfer. The server can't do this on the
  // presigned path -- the bytes never reach it -- so a video renamed
  // report.pdf would otherwise upload cleanly and only fail once the
  // indexing pipeline choked on it, with no clear reason shown to the user.
  const contentError = await verifyFileContent(file);
  if (contentError) throw new Error(contentError);

  return new Promise((resolve, reject) => {
    const form: FormData = new FormData();
    for (const [name, value] of Object.entries(fields)) form.append(name, value);
    // Must be last: S3 ignores every form field that follows the file part.
    form.append("file", file);

    const xhr = new XMLHttpRequest();
    let stallTimer: ReturnType<typeof setTimeout>;
    const armStall = () => {
      clearTimeout(stallTimer);
      stallTimer = setTimeout(() => {
        // A stall before any progress ever fired is the same "host
        // unreachable" shape as xhr.onerror below, just hanging instead of
        // failing fast -- some networks time out a connection attempt to
        // an unreachable host instead of refusing it.
        console.error("[uploadFileToS3] upload stalled", { url, origin: safeOrigin(url) });
        reject(new Error(`Upload stalled (no progress for ${stallMs / 1000}s)`));
        xhr.abort();
      }, stallMs);
    };
    armStall();
    xhr.open("POST", url);
    // No Content-Type header set by hand — the browser has to supply the
    // multipart boundary, and the policy deliberately doesn't bind the type.

    xhr.upload.onprogress = (e) => {
      armStall();
      if (e.lengthComputable) {
        onProgress?.(Number(((e.loaded / e.total) * 100).toFixed(1)));
      }
    };

    // Upload progress stops once the body is sent; switch to a fixed window
    // for the server response so a hung gateway cannot leave the promise pending.
    xhr.upload.onload = () => {
      clearTimeout(stallTimer);
      stallTimer = setTimeout(() => {
        reject(
          new Error(
            `Upload response timed out (no response for ${UPLOAD_RESPONSE_TIMEOUT_MS / 1000}s)`,
          ),
        );
        xhr.abort();
      }, UPLOAD_RESPONSE_TIMEOUT_MS);
    };

    xhr.onload = () => {
      clearTimeout(stallTimer);
      // POST Object answers 204 No Content on success, not 200.
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve();
        return;
      }
      reject(new Error(s3ErrorMessage(xhr)));
    };
    xhr.onerror = () => {
      clearTimeout(stallTimer);
      // The Event carries no detail -- browsers deliberately withhold DNS
      // failure / connection refused / CORS block from script, so this is
      // the one point that can log *which* host actually failed. Exactly
      // what fires when a presigned URL's host isn't reachable from the
      // browser (e.g. it points at the server's own "localhost" instead of
      // a browser-reachable one -- see S3_PUBLIC_ENDPOINT).
      console.error("[uploadFileToS3] network error reaching presigned URL", {
        url,
        origin: safeOrigin(url),
        status: xhr.status,
        readyState: xhr.readyState,
        responseURL: xhr.responseURL,
      });
      reject(
        new Error(
          `Could not reach the upload server at ${safeOrigin(url)}. ` + `Please contact support.`,
        ),
      );
    };
    xhr.onabort = () => {
      clearTimeout(stallTimer);
      reject(new Error("Upload aborted"));
    };
    xhr.send(form);
  });
}

/**
 * Turns an S3 error response into something a user can act on, from the XML
 * body's <Code> — without this, a rejected file surfaces as a bare "400 Bad
 * Request".
 */
function s3ErrorMessage(xhr: XMLHttpRequest): string {
  const code = /<Code>([^<]+)<\/Code>/.exec(xhr.responseText)?.[1] ?? "";
  switch (code) {
    case "EntityTooLarge":
      // Could be the per-file cap or the plan's remaining storage quota —
      // the signed policy uses whichever is lower, so this one code covers
      // both and there's no way to tell which from the S3 response alone.
      return "File is larger than your plan allows — check your per-file limit and remaining storage.";
    case "EntityTooSmall":
      return "File is empty.";
    case "AccessDenied":
      // The signature/policy was rejected — a stale (>5 min) authorization is
      // by far the most likely cause, and retrying gets a fresh one.
      return "Upload authorization expired or was rejected; please try again.";
    default:
      return `${xhr.status} ${xhr.statusText}: ${xhr.responseText}`;
  }
}
