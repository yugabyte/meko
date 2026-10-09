// How a client verifies an endpoint's server certificate, and the connection
// strings that follow from it.

import type { EndpointCaBundle } from "@/lib/api/types";

/** How a client verifies the documentdb gateway. `ca_file`: a private CA the
 *  client must be given. `system`: publicly trusted, nothing to hand over.
 *  `unverified`: the gateway self-signs, so no client can verify it. */
export type DocumentdbTrust = "ca_file" | "system" | "unverified";

/** A controller older than `documentdb_verifiable` always rendered the gateway
 *  self-signing; its `required` describes Postgres's certificate, not the
 *  gateway's. So an absent answer is `unverified`. */
export function documentdbTrust(bundle: EndpointCaBundle): DocumentdbTrust {
  if (!bundle.documentdb_verifiable) return "unverified";
  return bundle.required ? "ca_file" : "system";
}

/** The name the CA download is saved under, and so the path a connection
 *  string can point at. */
export function caFileName(endpointId: string): string {
  return `${endpointId}-ca.crt`;
}

/** The gateway always speaks TLS; the trust answer decides the rest. */
export function documentdbURI(address: string, endpointId: string, trust: DocumentdbTrust): string {
  const base = `mongodb://${address}/?tls=true`;
  switch (trust) {
    case "ca_file":
      return `${base}&tlsCAFile=${caFileName(endpointId)}`;
    case "unverified":
      return `${base}&tlsAllowInvalidCertificates=true`;
    case "system":
      return base;
  }
}

export function downloadCaFile(endpointId: string, pem: string): void {
  const url = URL.createObjectURL(new Blob([pem], { type: "application/x-pem-file" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = caFileName(endpointId);
  a.click();
  URL.revokeObjectURL(url);
}
