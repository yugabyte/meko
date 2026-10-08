import { describe, it, expect } from "vitest";
import { documentdbTrust, documentdbURI } from "./endpointTls";

// The controller's /ca answer is the only thing that tells a private CA, a
// public certificate and a self-signing gateway apart; the documentdb string
// differs for each.
describe("documentdbTrust", () => {
  it("reads the shapes of the /ca answer", () => {
    expect(documentdbTrust({ required: true, documentdb_verifiable: true, ca_crt: "pem" })).toBe("ca_file");
    expect(documentdbTrust({ required: false, documentdb_verifiable: true })).toBe("system");
    expect(documentdbTrust({ required: false, documentdb_verifiable: false })).toBe("unverified");
    // Postgres has a CA-signed certificate, but this gateway still self-signs.
    expect(documentdbTrust({ required: true, documentdb_verifiable: false, ca_crt: "pem" })).toBe("unverified");
  });

  // Such a controller always rendered the gateway self-signing; its `required`
  // is about Postgres's certificate.
  it("treats a controller that predates `documentdb_verifiable` as unverified", () => {
    expect(documentdbTrust({ required: true, ca_crt: "pem" })).toBe("unverified");
    expect(documentdbTrust({ required: false })).toBe("unverified");
  });
});

describe("documentdbURI", () => {
  const addr = "ep-1.amp.example.com:10260";

  it("points tlsCAFile at the name the CA download is saved under", () => {
    expect(documentdbURI(addr, "ep-1", "ca_file")).toBe(
      "mongodb://ep-1.amp.example.com:10260/?tls=true&tlsCAFile=ep-1-ca.crt",
    );
  });

  it("needs nothing beyond tls=true for a publicly-trusted certificate", () => {
    expect(documentdbURI(addr, "ep-1", "system")).toBe(
      "mongodb://ep-1.amp.example.com:10260/?tls=true",
    );
  });

  it("turns verification off only when the gateway self-signs", () => {
    expect(documentdbURI(addr, "ep-1", "unverified")).toBe(
      "mongodb://ep-1.amp.example.com:10260/?tls=true&tlsAllowInvalidCertificates=true",
    );
  });
});
