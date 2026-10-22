// The memory engine's client for the Go server's store API on run/store.sock: HTTP/JSON over a unix
// socket, with the per-start internal token. Every database call the memory engine makes goes through it.
import http from "node:http";

/** A failure the store API reported, with its code (NOT_FOUND, MODEL_CHANGED, ...). */
export class StoreError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

export class StoreClient {
  constructor(
    private readonly socket: string,
    private readonly token: string,
  ) {}

  /** Sends one request and returns the reply's data, or throws a StoreError. */
  request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const payload = body === undefined ? undefined : JSON.stringify(body);
    return new Promise((resolve, reject) => {
      const req = http.request(
        {
          socketPath: this.socket,
          method,
          path,
          headers: {
            authorization: `Bearer ${this.token}`,
            host: "store",
            ...(payload && { "content-type": "application/json", "content-length": Buffer.byteLength(payload) }),
          },
        },
        (res) => {
          const chunks: Buffer[] = [];
          res.on("data", (c: Buffer) => chunks.push(c));
          res.on("end", () => {
            let parsed: { data?: T; error?: { code: string; message: string } };
            try {
              parsed = JSON.parse(Buffer.concat(chunks).toString("utf8"));
            } catch {
              reject(new StoreError(res.statusCode ?? 0, "BAD_REPLY", `store answered ${res.statusCode} with no JSON`));
              return;
            }
            if (parsed.error) {
              reject(new StoreError(res.statusCode ?? 0, parsed.error.code, parsed.error.message));
              return;
            }
            resolve(parsed.data as T);
          });
          res.on("error", reject);
        },
      );
      req.on("error", reject);
      req.end(payload);
    });
  }
}
