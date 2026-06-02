/**
 * Content SHA-256 for knowledge-base uploads.
 *
 * `crypto.subtle.digest` only works in a [secure context](https://developer.mozilla.org/en-US/docs/Web/Security/Secure_Contexts)
 * (HTTPS or `http://localhost`). On plain HTTP (e.g. `http://10.150.1.75:5000`) we use the
 * pure-JS implementation from https://www.movable-type.co.uk/scripts/sha256.html
 */

/* - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -  */
/* SHA-256 (FIPS 180-4) implementation in JavaScript                  (c) Chris Veness 2002-2019  */
/*                                                                                   MIT Licence  */
/* www.movable-type.co.uk/scripts/sha256.html                                                     */
/* - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -  */

/**
 * SHA-256 hash function reference implementation.
 *
 * @see https://www.movable-type.co.uk/scripts/sha256.html
 * @see https://github.com/chrisveness/crypto/blob/master/sha256.js
 */
class Sha256 {
  /** SHA-256 of raw file bytes (no UTF-8 re-encoding). */
  static hashBytes(bytes: Uint8Array): string {
    // Build a one-char-per-byte binary string (code point === byte value).
    // `TextDecoder("latin1")` cannot be used: that label decodes as
    // Windows-1252, so bytes 0x80-0x9F map to the wrong code points and
    // corrupt the hash for binary files. Chunked String.fromCharCode keeps
    // this fast for multi-MB files while staying byte-exact.
    let msg = "";
    const chunkSize = 0x8000;
    for (let i = 0; i < bytes.length; i += chunkSize) {
      msg += String.fromCharCode.apply(null, Array.from(bytes.subarray(i, i + chunkSize)));
    }
    return Sha256.hash(msg, { msgFormat: "bytes" });
  }

  /**
   * Generates SHA-256 hash of string.
   *
   * @param msg - string to be hashed (format depends on `options.msgFormat`).
   * @param options.msgFormat - `'string'` UTF-8 (default), `'hex-bytes'`, or `'bytes'` (one char per byte).
   * @param options.outFormat - `'hex'` (default) or `'hex-w'`.
   */
  static hash(
    msg: string,
    options?: { msgFormat?: "string" | "hex-bytes" | "bytes"; outFormat?: "hex" | "hex-w" },
  ): string {
    const opt = {
      msgFormat: options?.msgFormat ?? "string",
      outFormat: options?.outFormat ?? "hex",
    };

    // note use throughout this routine of 'n >>> 0' to coerce Number 'n' to unsigned 32-bit integer

    switch (opt.msgFormat) {
      default: // default is to convert string to UTF-8, as SHA only deals with byte-streams
      case "string":
        msg = utf8Encode(msg);
        break;
      case "hex-bytes":
        msg = hexBytesToString(msg);
        break; // mostly for running tests
      case "bytes":
        break; // msg is already a binary string (one char per byte)
    }

    // constants [§4.2.2]
    const K = [
      0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
      0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
      0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
      0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
      0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
      0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
      0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
      0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
    ];

    // initial hash value [§5.3.3]
    const H = [
      0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
    ];

    // PREPROCESSING [§6.2.1]

    msg += String.fromCharCode(0x80); // add trailing '1' bit (+ 0's padding) to string [§5.1.1]

    // convert string msg into 512-bit blocks (array of 16 32-bit integers) [§5.2.1]
    const l = msg.length / 4 + 2; // length (in 32-bit integers) of msg + ‘1’ + appended length
    const N = Math.ceil(l / 16); // number of 16-integer (512-bit) blocks required to hold 'l' ints
    const M = new Array<Array<number>>(N); // message M is N×16 array of 32-bit integers

    for (let i = 0; i < N; i++) {
      M[i] = new Array(16);
      for (let j = 0; j < 16; j++) {
        // encode 4 chars per integer (64 per block), big-endian encoding
        M[i][j] =
          (msg.charCodeAt(i * 64 + j * 4 + 0) << 24) |
          (msg.charCodeAt(i * 64 + j * 4 + 1) << 16) |
          (msg.charCodeAt(i * 64 + j * 4 + 2) << 8) |
          msg.charCodeAt(i * 64 + j * 4 + 3);
      } // note running off the end of msg is ok 'cos bitwise ops on NaN return 0
    }
    // add length (in bits) into final pair of 32-bit integers (big-endian) [§5.1.1]
    const lenHi = ((msg.length - 1) * 8) / Math.pow(2, 32);
    const lenLo = ((msg.length - 1) * 8) >>> 0;
    M[N - 1][14] = Math.floor(lenHi);
    M[N - 1][15] = lenLo;

    // HASH COMPUTATION [§6.2.2]

    for (let i = 0; i < N; i++) {
      const W = new Array<number>(64);

      // 1 - prepare message schedule 'W'
      for (let t = 0; t < 16; t++) W[t] = M[i][t];
      for (let t = 16; t < 64; t++) {
        W[t] = (Sha256.σ1(W[t - 2]) + W[t - 7] + Sha256.σ0(W[t - 15]) + W[t - 16]) >>> 0;
      }

      // 2 - initialise working variables a, b, c, d, e, f, g, h with previous hash value
      let a = H[0],
        b = H[1],
        c = H[2],
        d = H[3],
        e = H[4],
        f = H[5],
        g = H[6],
        h = H[7];

      // 3 - main loop (note '>>> 0' for 'addition modulo 2^32')
      for (let t = 0; t < 64; t++) {
        const T1 = h + Sha256.Σ1(e) + Sha256.Ch(e, f, g) + K[t] + W[t];
        const T2 = Sha256.Σ0(a) + Sha256.Maj(a, b, c);
        h = g;
        g = f;
        f = e;
        e = (d + T1) >>> 0;
        d = c;
        c = b;
        b = a;
        a = (T1 + T2) >>> 0;
      }

      // 4 - compute the new intermediate hash value (note '>>> 0' for 'addition modulo 2^32')
      H[0] = (H[0] + a) >>> 0;
      H[1] = (H[1] + b) >>> 0;
      H[2] = (H[2] + c) >>> 0;
      H[3] = (H[3] + d) >>> 0;
      H[4] = (H[4] + e) >>> 0;
      H[5] = (H[5] + f) >>> 0;
      H[6] = (H[6] + g) >>> 0;
      H[7] = (H[7] + h) >>> 0;
    }

    // convert H0..H7 to hex strings (with leading zeros)
    const separator = opt.outFormat == "hex-w" ? " " : "";
    return H.map((v) => ("00000000" + v.toString(16)).slice(-8)).join(separator);

    function utf8Encode(str: string) {
      try {
        return new TextEncoder()
          .encode(str)
          .reduce((prev, curr) => prev + String.fromCharCode(curr), "");
      } catch {
        return unescape(encodeURIComponent(str));
      }
    }

    function hexBytesToString(hexStr: string) {
      const str = hexStr.replace(" ", "");
      return str == ""
        ? ""
        : str.match(/.{2}/g)!.map((byte) => String.fromCharCode(parseInt(byte, 16))).join("");
    }
  }

  /** Rotates right (circular right shift) value x by n positions [§3.2.4]. */
  static ROTR(n: number, x: number) {
    return (x >>> n) | (x << (32 - n));
  }

  /** Logical functions [§4.1.2]. */
  static Σ0(x: number) {
    return Sha256.ROTR(2, x) ^ Sha256.ROTR(13, x) ^ Sha256.ROTR(22, x);
  }
  static Σ1(x: number) {
    return Sha256.ROTR(6, x) ^ Sha256.ROTR(11, x) ^ Sha256.ROTR(25, x);
  }
  static σ0(x: number) {
    return Sha256.ROTR(7, x) ^ Sha256.ROTR(18, x) ^ (x >>> 3);
  }
  static σ1(x: number) {
    return Sha256.ROTR(17, x) ^ Sha256.ROTR(19, x) ^ (x >>> 10);
  }
  static Ch(x: number, y: number, z: number) {
    return (x & y) ^ (~x & z);
  }
  static Maj(x: number, y: number, z: number) {
    return (x & y) ^ (x & z) ^ (y & z);
  }
}

/* - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -  */

/** Whether Web Crypto SHA-256 is expected to work (secure context + `crypto.subtle`). */
export function canUseWebCryptoSha256(): boolean {
  return (
    typeof crypto !== "undefined" &&
    !!crypto.subtle &&
    (typeof window === "undefined" || window.isSecureContext)
  );
}

/** True when the page is not a secure context (typical for `http://<lan-ip>`). */
export function isNonSecureHttpOrigin(): boolean {
  return typeof window !== "undefined" && !window.isSecureContext;
}

async function sha256HexWebCrypto(bytes: Uint8Array): Promise<string> {
  const hashBuffer = await crypto.subtle.digest("SHA-256", bytes);
  return Array.from(new Uint8Array(hashBuffer))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

/** Pure-JS fallback — https://www.movable-type.co.uk/scripts/sha256.html */
function sha256HexFallback(bytes: Uint8Array): string {
  return Sha256.hashBytes(bytes);
}

/** Returns a 64-character lowercase hex SHA-256 of the file bytes. */
export async function sha256Hex(file: File): Promise<string> {
  const bytes = new Uint8Array(await file.arrayBuffer());

  if (canUseWebCryptoSha256()) {
    try {
      return await sha256HexWebCrypto(bytes);
    } catch {
      // `subtle` present but digest failed — fall through to JS implementation
    }
  }

  return sha256HexFallback(bytes);
}
