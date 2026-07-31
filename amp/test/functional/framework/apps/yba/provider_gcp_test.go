package yba

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleGCPProviderSpec() GCPProviderSpec {
	return GCPProviderSpec{
		Name:            "ampqa-gcp",
		Project:         "yugabyte",
		Network:         "yugabyte-network",
		SSHUser:         "centos",
		SSHPort:         22,
		Arch:            "x86",
		Zone:            "us-west1-b",
		CredentialsJSON: `{"type":"service_account","project_id":"yugabyte","private_key":"SECRET"}`,
		CredentialsFile: "/tmp/sa.json",
		KeyPairName:     "yb-dev-gcp",
		KeyPairContent:  "-----BEGIN KEY-----\ngcp-private-key\n-----END KEY-----",
		Image:           "almalinux-8",
		Regions: []RegionSpec{{
			Code:  "us-west1",
			Zones: []ZoneSpec{{Name: "us-west1-b", Subnet: "subnet-us-west1"}},
		}},
	}
}

// TestAddGCPProviderPayload asserts the create payload carries the GCP cloudInfo
// (project + credentials JSON + destVpcId + useHostVPC + vpcType=EXISTING),
// regions→zones→subnets, the SSH user/port, and — the hygiene-critical part — the
// reused keypair with skipKeyValidateAndUpload (and NO key-minting request).
func TestAddGCPProviderPayload(t *testing.T) {
	f := newFake()
	defer f.close()

	var body map[string]any
	f.on("POST /api/v1/customers/cust1/providers", func(w http.ResponseWriter, r *http.Request) {
		body = captureBody(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-g", "resourceUUID": "prov-g"})
	})

	ref, err := f.client().AddGCPProvider(context.Background(), sampleGCPProviderSpec())
	if err != nil {
		t.Fatalf("add gcp provider: %v", err)
	}
	if ref.TaskUUID != "task-g" || ref.ResourceUUID != "prov-g" {
		t.Fatalf("ref = %+v", ref)
	}

	if body["code"] != "gcp" || body["name"] != "ampqa-gcp" {
		t.Errorf("code/name = %v/%v", body["code"], body["name"])
	}
	details, _ := body["details"].(map[string]any)
	cloudInfo, _ := details["cloudInfo"].(map[string]any)
	gcpInfo, _ := cloudInfo["gcp"].(map[string]any)
	if gcpInfo["gceProject"] != "yugabyte" {
		t.Errorf("gceProject = %v, want yugabyte", gcpInfo["gceProject"])
	}
	if gcpInfo["gceApplicationCredentials"] != sampleGCPProviderSpec().CredentialsJSON {
		t.Errorf("gceApplicationCredentials not passed through as a string")
	}
	if gcpInfo["destVpcId"] != "yugabyte-network" {
		t.Errorf("destVpcId = %v, want yugabyte-network", gcpInfo["destVpcId"])
	}
	if gcpInfo["useHostVPC"] != true || gcpInfo["vpcType"] != "EXISTING" {
		t.Errorf("useHostVPC/vpcType = %v/%v, want true/EXISTING", gcpInfo["useHostVPC"], gcpInfo["vpcType"])
	}
	if details["sshUser"] != "centos" || details["sshPort"] != float64(22) {
		t.Errorf("ssh = %v:%v", details["sshUser"], details["sshPort"])
	}

	// Regions → zones → subnets.
	regions, _ := body["regions"].([]any)
	if len(regions) != 1 {
		t.Fatalf("regions = %v, want 1", regions)
	}
	region0, _ := regions[0].(map[string]any)
	if region0["code"] != "us-west1" {
		t.Errorf("region code = %v", region0["code"])
	}
	zones, _ := region0["zones"].([]any)
	zone0, _ := zones[0].(map[string]any)
	if zone0["subnet"] != "subnet-us-west1" || zone0["code"] != "us-west1-b" {
		t.Errorf("zone = %v (code should default to name)", zone0)
	}

	// Reused keypair — the hygiene assertion.
	aak, ok := body["allAccessKeys"].([]any)
	if !ok || len(aak) != 1 {
		t.Fatalf("allAccessKeys missing/misshaped: %v", body["allAccessKeys"])
	}
	keyInfo, _ := aak[0].(map[string]any)["keyInfo"].(map[string]any)
	if keyInfo["keyPairName"] != "yb-dev-gcp" || keyInfo["skipKeyValidateAndUpload"] != true {
		t.Errorf("keyInfo = %v, want reused keypair with skipKeyValidateAndUpload", keyInfo)
	}
	if keyInfo["sshPrivateKeyContent"] != sampleGCPProviderSpec().KeyPairContent {
		t.Errorf("sshPrivateKeyContent not passed through")
	}

	// Optional image bundle, arch mapped to the YBA enum.
	bundles, _ := body["imageBundles"].([]any)
	if len(bundles) != 1 {
		t.Fatalf("imageBundles = %v, want 1 (image set)", bundles)
	}
	bd, _ := bundles[0].(map[string]any)["details"].(map[string]any)
	if bd["globalYbImage"] != "almalinux-8" || bd["arch"] != "x86_64" {
		t.Errorf("image bundle details = %v, want globalYbImage almalinux-8 + arch x86_64", bd)
	}

	// Guard: no key-minting request path was touched.
	f.mu.Lock()
	for _, req := range f.requests {
		if strings.Contains(req, "keys") || strings.Contains(req, "keypair") || strings.Contains(req, "key_pair") {
			t.Errorf("unexpected key-minting request path: %s", req)
		}
	}
	f.mu.Unlock()
}

// TestAddGCPProviderNoKeypairOmitsAllAccessKeys: with no keypair configured the
// payload carries no allAccessKeys block (YBA uses project/OS-login SSH keys) and
// no image bundle when no image is set.
func TestAddGCPProviderNoKeypairOmitsAllAccessKeys(t *testing.T) {
	f := newFake()
	defer f.close()
	var body map[string]any
	f.on("POST /api/v1/customers/cust1/providers", func(w http.ResponseWriter, r *http.Request) {
		body = captureBody(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "t", "resourceUUID": "p"})
	})
	spec := sampleGCPProviderSpec()
	spec.KeyPairName, spec.KeyPairContent, spec.Image = "", "", ""
	if _, err := f.client().AddGCPProvider(context.Background(), spec); err != nil {
		t.Fatalf("add gcp provider: %v", err)
	}
	if _, ok := body["allAccessKeys"]; ok {
		t.Errorf("allAccessKeys present with no keypair configured")
	}
	if _, ok := body["imageBundles"]; ok {
		t.Errorf("imageBundles present with no image configured")
	}
}

// TestAddGCPProviderValidations: missing name/project/credentials/regions each
// error before any request is sent.
func TestAddGCPProviderValidations(t *testing.T) {
	f := newFake()
	defer f.close()
	base := sampleGCPProviderSpec()
	cases := map[string]func(GCPProviderSpec) GCPProviderSpec{
		"no name":    func(s GCPProviderSpec) GCPProviderSpec { s.Name = ""; return s },
		"no project": func(s GCPProviderSpec) GCPProviderSpec { s.Project = ""; return s },
		"no creds":   func(s GCPProviderSpec) GCPProviderSpec { s.CredentialsJSON = ""; return s },
		"no regions": func(s GCPProviderSpec) GCPProviderSpec { s.Regions = nil; return s },
	}
	for name, mut := range cases {
		if _, err := f.client().AddGCPProvider(context.Background(), mut(base)); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

// TestEnsureGCPProviderReusesExisting: an existing gcp provider by name is reused
// (create endpoint not hit).
func TestEnsureGCPProviderReusesExisting(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"uuid": "prov-existing", "code": "gcp", "name": "ampqa-gcp"},
		})
	})
	f.on("POST /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("create endpoint hit despite existing provider")
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "x"})
	})
	uuid, err := f.client().EnsureGCPProvider(context.Background(), sampleGCPProviderSpec())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if uuid != "prov-existing" {
		t.Errorf("uuid = %q, want prov-existing (reused)", uuid)
	}
	if sawRequest(f, "POST /api/v1/customers/cust1/providers") {
		t.Errorf("create endpoint was hit; expected reuse")
	}
}

// TestEnsureGCPProviderCreatesThenWaits: no existing provider → create is hit,
// the task is waited, and the provider uuid returned.
func TestEnsureGCPProviderCreatesThenWaits(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{map[string]any{"uuid": "other", "code": "aws", "name": "someone-else"}})
	})
	var created bool
	f.on("POST /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		created = true
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-g", "resourceUUID": "prov-new"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-g", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": TaskSuccess})
	})
	uuid, err := f.client().EnsureGCPProvider(context.Background(), sampleGCPProviderSpec())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if uuid != "prov-new" || !created {
		t.Errorf("uuid=%q created=%v, want prov-new/true", uuid, created)
	}
}

// TestGCPUserIntent asserts the gcp universe userIntent: providerType gcp, the
// GCE machine type, RF/nodes, a persistent-disk deviceInfo (storageType
// Persistent + storageClass standard, NO diskIops/throughput), accessKeyCode, and
// GCP-sanitized instance labels.
func TestGCPUserIntent(t *testing.T) {
	s := UniverseSpec{
		NamePrefix:        "ampqa",
		RunTag:            "abc123",
		ProviderCode:      "gcp",
		ProviderUUID:      "prov-g",
		AccessKeyCode:     "yb-dev-gcp-key",
		Region:            "us-west1",
		RegionUUIDs:       []string{"region-g"},
		InstanceType:      "n2-standard-4",
		NumNodes:          1,
		ReplicationFactor: 1,
		YBSoftwareVersion: "2025.2.1.0",
		VolumeSizeGB:      100,
		VolumeCount:       1,
		StorageType:       "Persistent",
		InstanceTags:      map[string]string{"yb_owner": "TestOwner", "Bad.Key": "Val UE"},
	}
	req := BuildConfigureRequest(s)
	clusters, _ := req["clusters"].([]any)
	ui, _ := clusters[0].(map[string]any)["userIntent"].(map[string]any)

	if ui["providerType"] != "gcp" {
		t.Errorf("providerType = %v, want gcp", ui["providerType"])
	}
	if ui["instanceType"] != "n2-standard-4" {
		t.Errorf("instanceType = %v, want n2-standard-4", ui["instanceType"])
	}
	if ui["provider"] != "prov-g" || ui["accessKeyCode"] != "yb-dev-gcp-key" {
		t.Errorf("provider/accessKeyCode = %v/%v", ui["provider"], ui["accessKeyCode"])
	}
	di, _ := ui["deviceInfo"].(map[string]any)
	if di["storageType"] != "Persistent" || di["storageClass"] != "standard" {
		t.Errorf("deviceInfo storage = %v/%v, want Persistent/standard", di["storageType"], di["storageClass"])
	}
	if _, ok := di["diskIops"]; ok {
		t.Errorf("gcp deviceInfo must NOT carry diskIops")
	}
	if _, ok := di["throughput"]; ok {
		t.Errorf("gcp deviceInfo must NOT carry throughput")
	}
	if ui["universeName"] != "ampqa-abc123" {
		t.Errorf("universeName = %v, want ampqa-abc123", ui["universeName"])
	}
	// GCE labels: keys lowercased/legalized, values lowercased+sanitized, the
	// per-run tag added.
	tags, _ := ui["instanceTags"].(map[string]any)
	if tags["yb_owner"] != "testowner" {
		t.Errorf("label yb_owner = %v, want lowercased testowner", tags["yb_owner"])
	}
	if tags["ampqa-run"] != "abc123" {
		t.Errorf("label ampqa-run = %v, want abc123", tags["ampqa-run"])
	}
	if _, ok := tags["Bad.Key"]; ok {
		t.Errorf("raw key Bad.Key must be sanitized, not passed through: %v", tags)
	}
	if v, ok := tags["bad_key"]; !ok || v != "val_ue" {
		t.Errorf("sanitized bad_key = %v (ok=%v), want val_ue", v, ok)
	}
}

// TestBuildGCPProviderSpecResolvesCredsPathAndKeypair: the credentials secret's
// Value is treated as a PATH to the SA JSON whose CONTENTS become
// CredentialsJSON (and the path is retained for the reap), and the keypair
// private-key CONTENTS are read off disk.
func TestBuildGCPProviderSpecResolvesCredsPathAndKeypair(t *testing.T) {
	dir := t.TempDir()
	saPath := filepath.Join(dir, "sa.json")
	saJSON := `{"type":"service_account","project_id":"yugabyte"}`
	if err := os.WriteFile(saPath, []byte(saJSON), 0o600); err != nil {
		t.Fatalf("write sa: %v", err)
	}
	keyPath := filepath.Join(dir, "gcp.pem")
	if err := os.WriteFile(keyPath, []byte("GCP-PRIVATE-KEY"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	cfg := fakeConfig{m: map[string]any{
		"apps.yba.gcp.credentials_secret": "gcp_key",
		"apps.yba.gcp.keypair_name":       "yb-dev-gcp",
		"apps.yba.gcp.keypair_file":       keyPath,
	}}
	// The resolver maps the secret NAME to the SA JSON file PATH (the env-variant
	// convention: Value == the path).
	resolve := func(name string) (string, error) {
		if name == "gcp_key" {
			return saPath, nil
		}
		return "", errUnknownSecret(name)
	}
	spec, err := BuildGCPProviderSpec(cfg, resolve)
	if err != nil {
		t.Fatalf("build spec: %v", err)
	}
	if spec.CredentialsJSON != saJSON {
		t.Errorf("CredentialsJSON = %q, want the file contents", spec.CredentialsJSON)
	}
	if spec.CredentialsFile != saPath {
		t.Errorf("CredentialsFile = %q, want the resolved path %q", spec.CredentialsFile, saPath)
	}
	if spec.KeyPairName != "yb-dev-gcp" || spec.KeyPairContent != "GCP-PRIVATE-KEY" {
		t.Errorf("keypair = %q / %q", spec.KeyPairName, spec.KeyPairContent)
	}
	// Region defaults to the shared us-west1 / us-west1-b when none configured.
	if len(spec.Regions) != 1 || spec.Regions[0].Code != DefaultGCPRegion {
		t.Fatalf("regions = %+v, want default us-west1", spec.Regions)
	}
	z := spec.Regions[0].Zones
	if len(z) != 1 || z[0].Name != DefaultGCPZone || z[0].Subnet != DefaultGCPSubnetwork {
		t.Errorf("default zone = %+v", z)
	}
}

// TestBuildGCPProviderSpecMissingCredSecretErrors: an unresolvable credentials
// secret is a hard error (YBA stores the creds — no ambient fallback).
func TestBuildGCPProviderSpecMissingCredSecretErrors(t *testing.T) {
	cfg := fakeConfig{m: map[string]any{}}
	resolve := func(name string) (string, error) { return "", errUnknownSecret(name) }
	if _, err := BuildGCPProviderSpec(cfg, resolve); err == nil {
		t.Fatalf("expected error when credentials secret unresolved")
	}
}

// TestBuildGCPUniverseSpecReleaseURLUsesGCPArch: the derived release tarball URL
// uses the GCP arch (x86 → x86_64), not the AWS arch.
func TestBuildGCPUniverseSpecReleaseURLUsesGCPArch(t *testing.T) {
	cfg := fakeConfig{m: map[string]any{
		"versions.yugabytedb": "2025.2.1.0-b42",
		"apps.yba.aws.arch":   "arm",
		"apps.yba.gcp.arch":   "x86",
	}}
	s := BuildGCPUniverseSpec(cfg)
	if s.ProviderCode != "gcp" {
		t.Errorf("ProviderCode = %q, want gcp", s.ProviderCode)
	}
	if !strings.Contains(s.ReleaseURL, "x86_64") || strings.Contains(s.ReleaseURL, "aarch64") {
		t.Errorf("release url = %q, want the x86_64 tarball (GCP arch)", s.ReleaseURL)
	}
	if s.YBSoftwareVersion != "2025.2.1.0-b42" {
		t.Errorf("version = %q", s.YBSoftwareVersion)
	}
}

func errUnknownSecret(name string) error { return &secretErr{name} }

type secretErr struct{ name string }

func (e *secretErr) Error() string { return "unknown secret " + e.name }
