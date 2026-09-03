package deploy

import (
	"strings"
	"testing"
)

// TestEndpointImage pins how a per-endpoint version becomes a Docker image
// reference. Unlike Kubernetes, the configured image is one full reference, so
// honouring a version means rewriting a tag out of it — and the naive
// last-colon split is wrong for a registry port, a digest, and an untagged
// reference, all of which are legal --deploy-endpoint-image values.
func TestEndpointImage(t *testing.T) {
	const digestRef = "quay.io/yugabyte/yb-amp-endpoint@sha256:0123456789abcdef"
	for _, tc := range []struct {
		name        string
		ref         string
		version     string
		wantImage   string
		wantVersion string
		wantErr     bool
	}{{
		// No version: the configured reference is run byte for byte, and its own
		// tag is what the endpoint is recorded as running.
		name: "no version keeps the configured reference", ref: "yb-amp/neon-endpoint:dev",
		wantImage: "yb-amp/neon-endpoint:dev", wantVersion: "dev",
	}, {
		name: "version replaces the tag", ref: "yb-amp/neon-endpoint:dev", version: "v2",
		wantImage: "yb-amp/neon-endpoint:v2", wantVersion: "v2",
	}, {
		// The trap: the last colon here introduces the registry port, not a tag.
		name: "registry port with a tag", ref: "localhost:5000/img:tag", version: "v2",
		wantImage: "localhost:5000/img:v2", wantVersion: "v2",
	}, {
		name: "registry port with no tag", ref: "localhost:5000/img", version: "v2",
		wantImage: "localhost:5000/img:v2", wantVersion: "v2",
	}, {
		// Untagged, so docker resolves :latest — which is not a version this
		// controller can name, so it reports none.
		name: "no tag and no version", ref: "localhost:5000/img",
		wantImage: "localhost:5000/img", wantVersion: "",
	}, {
		name: "bare name gains a tag", ref: "neon-endpoint", version: "v2",
		wantImage: "neon-endpoint:v2", wantVersion: "v2",
	}, {
		// The digest is what selects the image; retagging it would deploy the
		// pinned image under a new recorded version, so refuse instead.
		name: "digest reference refuses a version", ref: digestRef, version: "v2", wantErr: true,
	}, {
		name: "tag and digest refuses a version", ref: "yb-amp/neon-endpoint:v1@sha256:0123456789abcdef",
		version: "v2", wantErr: true,
	}, {
		// A version that is not a legal tag has to be refused HERE, because the
		// caller removes the running container before `docker run`: a reference that
		// only fails at run time costs the endpoint its container with nothing left
		// to bring it back.
		name: "version with a slash is refused", ref: "yb-amp/neon-endpoint:dev", version: "a/b", wantErr: true,
	}, {
		name: "version with a colon is refused", ref: "yb-amp/neon-endpoint:dev", version: "a:b", wantErr: true,
	}, {
		name: "version with a digest separator is refused", ref: "yb-amp/neon-endpoint:dev", version: "a@b", wantErr: true,
	}, {
		name: "version starting with a period is refused", ref: "yb-amp/neon-endpoint:dev", version: ".v2", wantErr: true,
	}, {
		name: "version starting with a hyphen is refused", ref: "yb-amp/neon-endpoint:dev", version: "-v2", wantErr: true,
	}, {
		name: "over-long version is refused", ref: "yb-amp/neon-endpoint:dev", version: strings.Repeat("v", 129), wantErr: true,
	}, {
		// The real shape of a version, which must keep working: MAJOR.YYMMDD.PATCH
		// with a short-commit suffix (ac-versioning.md).
		name: "a real release version is accepted", ref: "yb-amp/neon-endpoint:dev", version: "0.260814.0-71fe562d7",
		wantImage: "yb-amp/neon-endpoint:0.260814.0-71fe562d7", wantVersion: "0.260814.0-71fe562d7",
	}, {
		// A digest-pinned deployer still works; it just has no version to report,
		// since the digest is deliberately not modelled as one.
		name: "digest reference with no version", ref: digestRef,
		wantImage: digestRef, wantVersion: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			image, version, err := endpointImage(tc.ref, tc.version)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("endpointImage(%q, %q) = %q, want an error", tc.ref, tc.version, image)
				}
				return
			}
			if err != nil {
				t.Fatalf("endpointImage(%q, %q): %v", tc.ref, tc.version, err)
			}
			if image != tc.wantImage {
				t.Errorf("image = %q, want %q", image, tc.wantImage)
			}
			if version != tc.wantVersion {
				t.Errorf("reported version = %q, want %q", version, tc.wantVersion)
			}
		})
	}
}

// The local driver places nothing, so the version it reports is the one it was
// asked for: the only version it can name, and what keeps an upgrade of a
// locally-launched endpoint from being re-selected forever.
func TestLocalDeployerEchoesVersion(t *testing.T) {
	ep, err := LocalDeployer{}.CreateEndpoint(t.Context(), EndpointSpec{Name: "yb-ep-1", Version: "v2"})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if ep.Version != "v2" {
		t.Errorf("Version = %q, want v2", ep.Version)
	}
	none, err := LocalDeployer{}.CreateEndpoint(t.Context(), EndpointSpec{Name: "yb-ep-2"})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if none.Version != "" {
		t.Errorf("Version = %q, want empty (nothing was asked for and nothing was placed)", none.Version)
	}
}
