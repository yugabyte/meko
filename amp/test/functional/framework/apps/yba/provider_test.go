package yba

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// captureBody decodes a request body into a map for assertions.
func captureBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var m map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("decode body: %v (%s)", err, data)
		}
	}
	return m
}

func sawRequest(f *fakeYBA, key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r == key {
			return true
		}
	}
	return false
}

func sampleProviderSpec() ProviderSpec {
	return ProviderSpec{
		Cloud:          "aws",
		Name:           "ampqa-aws",
		SSHUser:        "ec2-user",
		Arch:           "arm",
		KeyPairName:    "yb-dev-aws-2",
		KeyPairContent: "-----BEGIN KEY-----\nsecret-key-material\n-----END KEY-----",
		AccessKey:      "AKIAEXAMPLE",
		SecretKey:      "supersecretvalue",
		Regions: []RegionSpec{{
			Code:            "us-west-2",
			SecurityGroupID: "sg-123",
			VPCID:           "vpc-123",
			AMI:             "ami-123",
			Zones:           []ZoneSpec{{Name: "us-west-2a", Subnet: "subnet-abc"}},
		}},
	}
}

// TestAddAWSProviderPayloadReusesKeypair asserts the create payload carries the
// existing keyPairName + private-key content with skipKeyValidateAndUpload (so
// YBA never mints a key), the resolved creds, and the regions/zones/subnets —
// and that NO create-key-pair request is ever emitted.
func TestAddAWSProviderPayloadReusesKeypair(t *testing.T) {
	f := newFake()
	defer f.close()

	var body map[string]any
	f.on("POST /api/v1/customers/cust1/providers", func(w http.ResponseWriter, r *http.Request) {
		body = captureBody(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-p", "resourceUUID": "prov-1"})
	})

	ref, err := f.client().AddAWSProvider(context.Background(), sampleProviderSpec())
	if err != nil {
		t.Fatalf("add aws provider: %v", err)
	}
	if ref.TaskUUID != "task-p" || ref.ResourceUUID != "prov-1" {
		t.Fatalf("ref = %+v", ref)
	}

	// allAccessKeys reuses the existing keypair — the single most important
	// hygiene assertion of this wave.
	aak, ok := body["allAccessKeys"].([]any)
	if !ok || len(aak) != 1 {
		t.Fatalf("allAccessKeys missing/misshaped: %v", body["allAccessKeys"])
	}
	keyInfo, _ := aak[0].(map[string]any)["keyInfo"].(map[string]any)
	if keyInfo["keyPairName"] != "yb-dev-aws-2" {
		t.Errorf("keyPairName = %v, want yb-dev-aws-2", keyInfo["keyPairName"])
	}
	if keyInfo["sshPrivateKeyContent"] != sampleProviderSpec().KeyPairContent {
		t.Errorf("sshPrivateKeyContent not passed through")
	}
	if keyInfo["skipKeyValidateAndUpload"] != true {
		t.Errorf("skipKeyValidateAndUpload = %v, want true (prevents YBA minting a key)", keyInfo["skipKeyValidateAndUpload"])
	}

	// Resolved creds injected into the cloudInfo block.
	details, _ := body["details"].(map[string]any)
	cloudInfo, _ := details["cloudInfo"].(map[string]any)
	awsInfo, _ := cloudInfo["aws"].(map[string]any)
	if awsInfo["awsAccessKeyID"] != "AKIAEXAMPLE" || awsInfo["awsAccessKeySecret"] != "supersecretvalue" {
		t.Errorf("creds not injected: %v", awsInfo)
	}

	// Regions → zones → subnets.
	regions, _ := body["regions"].([]any)
	if len(regions) != 1 {
		t.Fatalf("regions = %v, want 1", regions)
	}
	region0, _ := regions[0].(map[string]any)
	if region0["code"] != "us-west-2" {
		t.Errorf("region code = %v", region0["code"])
	}
	zones, _ := region0["zones"].([]any)
	zone0, _ := zones[0].(map[string]any)
	if zone0["subnet"] != "subnet-abc" || zone0["code"] != "us-west-2a" {
		t.Errorf("zone = %v (code should default to name)", zone0)
	}

	// arch "arm" must serialize as YBA's Architecture enum "aarch64"; YBA 400s
	// on the raw "arm" string.
	regionDetails, _ := region0["details"].(map[string]any)
	regionCloud, _ := regionDetails["cloudInfo"].(map[string]any)
	regionAWS, _ := regionCloud["aws"].(map[string]any)
	if regionAWS["arch"] != "aarch64" {
		t.Errorf("region arch = %v, want aarch64 (mapped from %q)", regionAWS["arch"], sampleProviderSpec().Arch)
	}
	// The region AMI must reach ybImage — an empty ybImage makes YBA default to
	// an x86_64 image that RunInstances rejects against an arm64 instance type.
	if regionAWS["ybImage"] != "ami-123" {
		t.Errorf("region ybImage = %v, want ami-123 (from spec AMI)", regionAWS["ybImage"])
	}

	// Guard: not a single key-minting request path was touched.
	f.mu.Lock()
	for _, req := range f.requests {
		if strings.Contains(req, "keys") || strings.Contains(req, "keypair") || strings.Contains(req, "key_pair") {
			t.Errorf("unexpected key-minting request path: %s", req)
		}
	}
	f.mu.Unlock()
}

// TestProviderAccessKeyCode returns the first access key's code — the value the
// universe userIntent must carry as accessKeyCode.
func TestProviderAccessKeyCode(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/providers/prov-1/access_keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"idKey": map[string]any{"keyCode": "yb-dev-aws-2-key", "providerUUID": "prov-1"}},
		})
	})
	code, err := f.client().ProviderAccessKeyCode(context.Background(), "prov-1")
	if err != nil {
		t.Fatalf("access key code: %v", err)
	}
	if code != "yb-dev-aws-2-key" {
		t.Errorf("code = %q, want yb-dev-aws-2-key", code)
	}
}

func TestProviderAccessKeyCodeNoneFails(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/providers/prov-1/access_keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{})
	})
	if _, err := f.client().ProviderAccessKeyCode(context.Background(), "prov-1"); err == nil {
		t.Fatalf("expected error when provider has no access key")
	}
}

// TestAddInstanceTypePayload asserts the instance-type registration POSTs the
// idKey (code + provider), the mapped arch enum, cores/mem, and an EBS volume
// entry — the fields YBA needs to resolve an ARM instance type at create.
func TestAddInstanceTypePayload(t *testing.T) {
	f := newFake()
	defer f.close()

	var body map[string]any
	f.on("POST /api/v1/customers/cust1/providers/prov-1/instance_types", func(w http.ResponseWriter, r *http.Request) {
		body = captureBody(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"instanceTypeCode": "m7g.xlarge"})
	})

	err := f.client().AddInstanceType(context.Background(), "prov-1", InstanceTypeSpec{
		Code: "m7g.xlarge", NumCores: 4, MemSizeGB: 16, Arch: "arm",
	})
	if err != nil {
		t.Fatalf("add instance type: %v", err)
	}
	idKey, _ := body["idKey"].(map[string]any)
	if idKey["instanceTypeCode"] != "m7g.xlarge" || idKey["providerUuid"] != "prov-1" {
		t.Errorf("idKey = %v", idKey)
	}
	if body["numCores"] != float64(4) || body["memSizeGB"] != float64(16) {
		t.Errorf("cores/mem = %v/%v", body["numCores"], body["memSizeGB"])
	}
	details, _ := body["instanceTypeDetails"].(map[string]any)
	if details["arch"] != "aarch64" {
		t.Errorf("arch = %v, want aarch64 (mapped from arm)", details["arch"])
	}
	vols, _ := details["volumeDetailsList"].([]any)
	if len(vols) != 1 {
		t.Fatalf("volumeDetailsList = %v", vols)
	}
	if v0, _ := vols[0].(map[string]any); v0["volumeType"] != "EBS" {
		t.Errorf("volume = %v", vols[0])
	}
}

func TestYBAArchMapsToEnum(t *testing.T) {
	cases := map[string]string{
		"arm": "aarch64", "arm64": "aarch64", "aarch64": "aarch64",
		"x86": "x86_64", "x86_64": "x86_64", "amd64": "x86_64",
		"ARM": "aarch64", " x86 ": "x86_64",
		"weird": "weird", // unknown passes through
	}
	for in, want := range cases {
		if got := ybaArch(in); got != want {
			t.Errorf("ybaArch(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEnsureAWSProviderCreatesThenWaits: no existing provider → create is hit,
// the task is waited to completion, and the provider uuid is returned.
func TestEnsureAWSProviderCreatesThenWaits(t *testing.T) {
	f := newFake()
	defer f.close()

	f.on("GET /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"uuid": "other", "code": "gcp", "name": "someone-else"},
		})
	})
	var created bool
	f.on("POST /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		created = true
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-p", "resourceUUID": "prov-new"})
	})
	var polls int
	f.on("GET /api/v1/customers/cust1/tasks/task-p", func(w http.ResponseWriter, _ *http.Request) {
		polls++
		status := TaskRunning
		if polls >= 2 {
			status = TaskSuccess
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": status})
	})

	uuid, err := f.client().EnsureAWSProvider(context.Background(), sampleProviderSpec())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if uuid != "prov-new" {
		t.Errorf("uuid = %q, want prov-new", uuid)
	}
	if !created {
		t.Errorf("create endpoint not hit")
	}
	if polls < 2 {
		t.Errorf("task not polled to completion (polls=%d)", polls)
	}
}

// TestEnsureAWSProviderReusesExisting: a provider with the configured name
// already exists → the create endpoint is NOT hit and its uuid is returned.
func TestEnsureAWSProviderReusesExisting(t *testing.T) {
	f := newFake()
	defer f.close()

	f.on("GET /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"uuid": "prov-existing", "code": "aws", "name": "ampqa-aws"},
		})
	})
	f.on("POST /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("create endpoint hit despite existing provider")
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "x"})
	})

	uuid, err := f.client().EnsureAWSProvider(context.Background(), sampleProviderSpec())
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

// TestAddProviderAzureNotImplemented: Azure returns a clear error and touches no
// endpoint (GCP is implemented — see TestAddProviderGCPDispatch).
func TestAddProviderAzureNotImplemented(t *testing.T) {
	f := newFake()
	defer f.close()
	for _, cloud := range []string{"azu", "azure"} {
		spec := sampleProviderSpec()
		spec.Cloud = cloud
		if _, err := f.client().AddProvider(context.Background(), spec); err == nil {
			t.Errorf("cloud %q: expected not-implemented error", cloud)
		}
	}
}

// TestAddProviderGCPRequiresSpec: cloud "gcp" with no GCP payload errors clearly
// rather than emitting a half-built request.
func TestAddProviderGCPRequiresSpec(t *testing.T) {
	f := newFake()
	defer f.close()
	spec := sampleProviderSpec()
	spec.Cloud = "gcp"
	spec.GCP = nil
	if _, err := f.client().AddProvider(context.Background(), spec); err == nil {
		t.Fatalf("expected error when cloud=gcp but spec.GCP is nil")
	}
}

// TestAddProviderGCPDispatch: cloud "gcp" with a GCP spec dispatches to the gcp
// provider-create endpoint and returns its task ref.
func TestAddProviderGCPDispatch(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/providers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-g", "resourceUUID": "prov-g"})
	})
	spec := ProviderSpec{Cloud: "gcp", GCP: &GCPProviderSpec{
		Name: "ampqa-gcp", Project: "yugabyte", CredentialsJSON: `{"type":"service_account"}`,
		Regions: []RegionSpec{{Code: "us-west1", Zones: []ZoneSpec{{Name: "us-west1-b", Subnet: "subnet-us-west1"}}}},
	}}
	ref, err := f.client().AddProvider(context.Background(), spec)
	if err != nil {
		t.Fatalf("add gcp provider via dispatch: %v", err)
	}
	if ref.TaskUUID != "task-g" || ref.ResourceUUID != "prov-g" {
		t.Fatalf("ref = %+v", ref)
	}
}

// TestRegionUUIDsForCodes resolves region UUIDs by code and errors on a miss.
func TestRegionUUIDsForCodes(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/providers/prov-1/regions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"uuid": "r-west", "code": "us-west-2", "name": "US West"},
			map[string]any{"uuid": "r-east", "code": "us-east-1", "name": "US East"},
		})
	})
	uuids, err := f.client().RegionUUIDsForCodes(context.Background(), "prov-1", "us-west-2")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(uuids) != 1 || uuids[0] != "r-west" {
		t.Errorf("uuids = %v, want [r-west]", uuids)
	}
	if _, err := f.client().RegionUUIDsForCodes(context.Background(), "prov-1", "eu-central-1"); err == nil {
		t.Errorf("expected error for unknown region code")
	}
}
