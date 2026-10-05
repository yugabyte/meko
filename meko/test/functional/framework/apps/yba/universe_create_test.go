package yba

import (
	"context"
	"net/http"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func sampleUniverseSpec() UniverseSpec {
	return UniverseSpec{
		NamePrefix:        "mekoqa",
		RunTag:            "run42",
		ProviderUUID:      "prov-1",
		ProviderCode:      "aws",
		RegionUUIDs:       []string{"r-west"},
		InstanceType:      "m7g.xlarge",
		NumNodes:          3,
		ReplicationFactor: 3,
		YBSoftwareVersion: "2025.2.1.0-b141",
		VolumeSizeGB:      100,
		VolumeCount:       1,
		StorageType:       "GP3",
		DiskIops:          3000,
		Throughput:        125,
		AccessKeyCode:     "yb-dev-aws-2-key",
		InstanceTags:      map[string]string{"yb_owner": "testowner", "yb_dept": "qa"},
	}
}

// TestBuildConfigureRequestShape asserts the CREATE configure request carries
// the framework-decided userIntent (nodes/RF/instance/region/version/volume) and
// the mekoqa-tagged universe name, under a single PRIMARY cluster.
func TestBuildConfigureRequestShape(t *testing.T) {
	req := BuildConfigureRequest(sampleUniverseSpec())

	if req["clusterOperation"] != "CREATE" || req["currentClusterType"] != "PRIMARY" {
		t.Errorf("op/type = %v/%v", req["clusterOperation"], req["currentClusterType"])
	}
	clusters, _ := req["clusters"].([]any)
	if len(clusters) != 1 {
		t.Fatalf("clusters = %v, want 1", clusters)
	}
	cl0, _ := clusters[0].(map[string]any)
	if cl0["clusterType"] != "PRIMARY" {
		t.Errorf("clusterType = %v", cl0["clusterType"])
	}
	ui, _ := cl0["userIntent"].(map[string]any)
	if ui["universeName"] != "mekoqa-run42" {
		t.Errorf("universeName = %v, want mekoqa-run42 (mekoqa-tagged)", ui["universeName"])
	}
	if ui["provider"] != "prov-1" || ui["providerType"] != "aws" {
		t.Errorf("provider = %v / %v", ui["provider"], ui["providerType"])
	}
	if ui["numNodes"] != 3 || ui["replicationFactor"] != 3 {
		t.Errorf("numNodes/RF = %v/%v", ui["numNodes"], ui["replicationFactor"])
	}
	if ui["instanceType"] != "m7g.xlarge" {
		t.Errorf("instanceType = %v", ui["instanceType"])
	}
	if ui["ybSoftwareVersion"] != "2025.2.1.0-b141" {
		t.Errorf("ybSoftwareVersion = %v", ui["ybSoftwareVersion"])
	}
	// accessKeyCode ties the universe to the provider keypair, without it YBA
	// launches nodes with KeyName=None.
	if ui["accessKeyCode"] != "yb-dev-aws-2-key" {
		t.Errorf("accessKeyCode = %v, want yb-dev-aws-2-key", ui["accessKeyCode"])
	}
	// instanceTags (map form) carry the ownership tags + the per-run mekoqa-run
	// tag; the governance SCP conditions RunInstances on them.
	tags, _ := ui["instanceTags"].(map[string]any)
	if tags["yb_owner"] != "testowner" || tags["yb_dept"] != "qa" {
		t.Errorf("instanceTags ownership = %v", tags)
	}
	if tags["mekoqa-run"] != "run42" {
		t.Errorf("instanceTags mekoqa-run = %v, want run42 (RunTag)", tags["mekoqa-run"])
	}
	rl, _ := ui["regionList"].([]any)
	if len(rl) != 1 || rl[0] != "r-west" {
		t.Errorf("regionList = %v, want [r-west]", rl)
	}
	di, _ := ui["deviceInfo"].(map[string]any)
	if di["volumeSize"] != 100 || di["numVolumes"] != 1 {
		t.Errorf("deviceInfo = %v", di)
	}
	// GP3 must carry diskIops + throughput, YBA 400s ("Disk IOPS is mandatory
	// for GP3 storage") without them.
	if di["storageType"] != "GP3" || di["diskIops"] != 3000 || di["throughput"] != 125 {
		t.Errorf("GP3 deviceInfo = %v, want storageType GP3 + diskIops 3000 + throughput 125", di)
	}
}

func sampleK8sUniverseSpec() UniverseSpec {
	return UniverseSpec{
		NamePrefix:        "mekoqa",
		RunTag:            "run42",
		ProviderUUID:      "k8s-prov-1",
		ProviderCode:      "kubernetes",
		RegionUUIDs:       []string{"r-uswest1"},
		NumNodes:          1,
		ReplicationFactor: 1,
		YBSoftwareVersion: "2025.2.1.0-b141",
		VolumeSizeGB:      50,
		VolumeCount:       1,
		K8sStorageClass:   "standard",
		MasterCPUCores:    2,
		MasterMemGiB:      4,
		TServerCPUCores:   2,
		TServerMemGiB:     4,
		// AWS-only fields set to prove the k8s path ignores them.
		InstanceType:  "m7g.xlarge",
		AccessKeyCode: "yb-dev-aws-2-key",
		StorageType:   "GP3",
		InstanceTags:  map[string]string{"yb_owner": "testowner"},
	}
}

// TestBuildConfigureRequestEncryptionInTransit asserts a plaintext spec carries
// no TLS fields (YBA's default), and a TLS spec carries the toggles, the root CA
// and the gflags on any provider's intent.
func TestBuildConfigureRequestEncryptionInTransit(t *testing.T) {
	// Set the gflags on the plaintext spec too, so their absence below is the
	// encryption gate's doing and not just an empty map.
	plain := sampleUniverseSpec()
	plain.MasterGFlags = map[string]string{"ysql_enable_auth": "true"}
	plain.TServerGFlags = map[string]string{"ysql_enable_auth": "true"}
	ui := userIntentOf(t, BuildConfigureRequest(plain))
	for _, k := range []string{"enableNodeToNodeEncrypt", "enableClientToNodeEncrypt", "rootCA"} {
		if _, ok := ui[k]; ok {
			t.Errorf("plaintext spec carries %s", k)
		}
	}

	s := sampleUniverseSpec()
	s.ProviderCode = "kubernetes"
	s.EnableNodeToNodeEncrypt = true
	s.EnableClientToNodeEncrypt = true
	s.RootCA = "cert-uuid"
	s.RootAndClientRootCASame = true
	s.TServerGFlags = map[string]string{"ysql_hba_conf_csv": "hostssl all all all cert"}
	ui = userIntentOf(t, BuildConfigureRequest(s))
	if ui["enableNodeToNodeEncrypt"] != true || ui["enableClientToNodeEncrypt"] != true {
		t.Errorf("toggles = %v / %v", ui["enableNodeToNodeEncrypt"], ui["enableClientToNodeEncrypt"])
	}
	if ui["rootCA"] != "cert-uuid" || ui["rootAndClientRootCASame"] != true {
		t.Errorf("rootCA = %v same=%v", ui["rootCA"], ui["rootAndClientRootCASame"])
	}
	gf, _ := ui["tserverGFlags"].(map[string]any)
	if gf["ysql_hba_conf_csv"] != "hostssl all all all cert" {
		t.Errorf("tserverGFlags = %v", ui["tserverGFlags"])
	}
	if _, ok := ui["masterGFlags"]; ok {
		t.Errorf("empty masterGFlags rendered: %v", ui["masterGFlags"])
	}
}

func userIntentOf(t *testing.T, req map[string]any) map[string]any {
	t.Helper()
	clusters, _ := req["clusters"].([]any)
	if len(clusters) != 1 {
		t.Fatalf("clusters = %v", clusters)
	}
	cl0, _ := clusters[0].(map[string]any)
	ui, _ := cl0["userIntent"].(map[string]any)
	return ui
}

// TestBuildConfigureRequestK8sShape asserts a kubernetes universe emits
// providerType=kubernetes with the master/tserver K8SNodeResourceSpec + a
// deviceInfo.storageClass, and NO AWS-only fields (instanceType, accessKeyCode,
// instanceTags, storageType/diskIops).
func TestBuildConfigureRequestK8sShape(t *testing.T) {
	ui := BuildConfigureRequest(sampleK8sUniverseSpec())["clusters"].([]any)[0].(map[string]any)["userIntent"].(map[string]any)

	if ui["providerType"] != "kubernetes" {
		t.Errorf("providerType = %v, want kubernetes", ui["providerType"])
	}
	if ui["provider"] != "k8s-prov-1" {
		t.Errorf("provider = %v", ui["provider"])
	}
	if ui["numNodes"] != 1 || ui["replicationFactor"] != 1 {
		t.Errorf("numNodes/RF = %v/%v", ui["numNodes"], ui["replicationFactor"])
	}
	if ui["ybSoftwareVersion"] != "2025.2.1.0-b141" {
		t.Errorf("ybSoftwareVersion = %v", ui["ybSoftwareVersion"])
	}
	// k8s does not assign a node-level public IP.
	if ui["assignPublicIP"] != false {
		t.Errorf("assignPublicIP = %v, want false", ui["assignPublicIP"])
	}
	// AWS-only fields must be absent.
	for _, k := range []string{"instanceType", "accessKeyCode", "instanceTags"} {
		if _, ok := ui[k]; ok {
			t.Errorf("k8s userIntent carries AWS-only field %q: %v", k, ui[k])
		}
	}
	m, _ := ui["masterK8SNodeResourceSpec"].(map[string]any)
	if m["cpuCoreCount"] != 2 || m["memoryGib"] != 4 {
		t.Errorf("masterK8SNodeResourceSpec = %v, want cpu 2 / mem 4", m)
	}
	ts, _ := ui["tserverK8SNodeResourceSpec"].(map[string]any)
	if ts["cpuCoreCount"] != 2 || ts["memoryGib"] != 4 {
		t.Errorf("tserverK8SNodeResourceSpec = %v, want cpu 2 / mem 4", ts)
	}
	di, _ := ui["deviceInfo"].(map[string]any)
	if di["volumeSize"] != 50 || di["numVolumes"] != 1 || di["storageClass"] != "standard" {
		t.Errorf("deviceInfo = %v, want volumeSize 50 / numVolumes 1 / storageClass standard", di)
	}
	// k8s deviceInfo must NOT carry the AWS EBS fields.
	for _, k := range []string{"storageType", "diskIops", "throughput"} {
		if _, ok := di[k]; ok {
			t.Errorf("k8s deviceInfo carries AWS-only field %q: %v", k, di[k])
		}
	}
	rl, _ := ui["regionList"].([]any)
	if len(rl) != 1 || rl[0] != "r-uswest1" {
		t.Errorf("regionList = %v, want [r-uswest1]", rl)
	}
}

// TestBuildConfigureRequestK8sResourceDefaults: zero resource specs fall back to
// the k8s defaults rather than emitting 0 (which YBA rejects, MIN_CPU 0.5).
func TestBuildConfigureRequestK8sResourceDefaults(t *testing.T) {
	s := sampleK8sUniverseSpec()
	s.MasterCPUCores, s.MasterMemGiB, s.TServerCPUCores, s.TServerMemGiB = 0, 0, 0, 0
	s.K8sStorageClass = ""
	ui := BuildConfigureRequest(s)["clusters"].([]any)[0].(map[string]any)["userIntent"].(map[string]any)
	m := ui["masterK8SNodeResourceSpec"].(map[string]any)
	if m["cpuCoreCount"] != DefaultK8sMasterCPUCores || m["memoryGib"] != DefaultK8sMasterMemGiB {
		t.Errorf("master defaults = %v, want %d/%d", m, DefaultK8sMasterCPUCores, DefaultK8sMasterMemGiB)
	}
	di := ui["deviceInfo"].(map[string]any)
	if di["storageClass"] != DefaultK8sStorageClass {
		t.Errorf("storageClass default = %v, want %q", di["storageClass"], DefaultK8sStorageClass)
	}
}

// TestBuildConfigureRequestAccessKeyOmitted: an empty AccessKeyCode leaves the
// field out entirely (YBA falls back to the provider default only when absent).
func TestBuildConfigureRequestAccessKeyOmitted(t *testing.T) {
	s := sampleUniverseSpec()
	s.AccessKeyCode = ""
	ui := BuildConfigureRequest(s)["clusters"].([]any)[0].(map[string]any)["userIntent"].(map[string]any)
	if _, ok := ui["accessKeyCode"]; ok {
		t.Errorf("accessKeyCode present with empty spec: %v", ui["accessKeyCode"])
	}
}

// TestBuildConfigureRequestStorageFields: GP3 emits diskIops+throughput, IO1
// emits diskIops only, and a baseline class (gp2) emits neither.
func TestBuildConfigureRequestStorageFields(t *testing.T) {
	device := func(s UniverseSpec) map[string]any {
		ui := BuildConfigureRequest(s)["clusters"].([]any)[0].(map[string]any)["userIntent"].(map[string]any)
		return ui["deviceInfo"].(map[string]any)
	}

	gp3 := device(sampleUniverseSpec())
	if _, ok := gp3["diskIops"]; !ok {
		t.Errorf("GP3 missing diskIops: %v", gp3)
	}
	if _, ok := gp3["throughput"]; !ok {
		t.Errorf("GP3 missing throughput: %v", gp3)
	}

	io1 := sampleUniverseSpec()
	io1.StorageType = "IO1"
	di := device(io1)
	if di["diskIops"] != 3000 {
		t.Errorf("IO1 diskIops = %v, want 3000", di["diskIops"])
	}
	if _, ok := di["throughput"]; ok {
		t.Errorf("IO1 must not carry throughput: %v", di)
	}

	gp2 := sampleUniverseSpec()
	gp2.StorageType = "Scratch"
	di = device(gp2)
	if _, ok := di["diskIops"]; ok {
		t.Errorf("non-GP3/IO class must not carry diskIops: %v", di)
	}
	if _, ok := di["throughput"]; ok {
		t.Errorf("non-GP3/IO class must not carry throughput: %v", di)
	}
}

// TestInstanceTagsFromConfigMergesDefaults: the mandatory ownership tags are
// always present; apps.yba.universe.instance_tags overrides/extends them.
func TestInstanceTagsFromConfigMergesDefaults(t *testing.T) {
	tags := instanceTagsFromConfig(fakeConfig{m: map[string]any{
		"apps.yba.universe.instance_tags": map[string]any{"yb_owner": "someone", "cost_center": "cc-1"},
	}})
	if tags["yb_owner"] != "someone" {
		t.Errorf("config override lost: yb_owner = %q", tags["yb_owner"])
	}
	if tags["cost_center"] != "cc-1" {
		t.Errorf("config extension lost: cost_center = %q", tags["cost_center"])
	}
	if tags["yb_dept"] != "qa" || tags["autoclean"] != "true" || tags["mekoqa"] != "1" {
		t.Errorf("mandatory defaults dropped: %v", tags)
	}
}

// TestBuildConfigureRequestVolumeFloor defaults a non-positive volume count to 1.
func TestBuildConfigureRequestVolumeFloor(t *testing.T) {
	s := sampleUniverseSpec()
	s.VolumeCount = 0
	ui := BuildConfigureRequest(s)["clusters"].([]any)[0].(map[string]any)["userIntent"].(map[string]any)
	di := ui["deviceInfo"].(map[string]any)
	if di["numVolumes"] != DefaultVolumeCount {
		t.Errorf("numVolumes = %v, want %d", di["numVolumes"], DefaultVolumeCount)
	}
}

// TestCreateUniverseFromSpecSingleCall drives the single-call flow: the userIntent
// reaches POST /universes/clusters and a clean {taskUUID, resourceUUID} is parsed.
func TestCreateUniverseFromSpecSingleCall(t *testing.T) {
	f := newFake()
	defer f.close()

	var sentUI map[string]any
	f.on("POST /api/v1/customers/cust1/universes/clusters", func(w http.ResponseWriter, r *http.Request) {
		body := captureBody(t, r)
		clusters, _ := body["clusters"].([]any)
		cl0, _ := clusters[0].(map[string]any)
		sentUI, _ = cl0["userIntent"].(map[string]any)
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-u", "resourceUUID": "u-new"})
	})

	ref, err := f.client().CreateUniverseFromSpec(context.Background(), sampleUniverseSpec())
	if err != nil {
		t.Fatalf("create from spec: %v", err)
	}
	if ref.ResourceUUID != "u-new" || ref.TaskUUID != "task-u" {
		t.Errorf("ref = %+v, want {task-u, u-new}", ref)
	}
	if sentUI["universeName"] != "mekoqa-run42" {
		t.Errorf("userIntent name = %v, want mekoqa-run42", sentUI["universeName"])
	}
	// The single call replaces the two-call configure→create.
	if sawRequest(f, "POST /api/v1/customers/cust1/universe_configure") {
		t.Errorf("legacy /universe_configure hit; expected only /universes/clusters")
	}
}

// TestCreateUniverseFromSpecError propagates a create-call error.
func TestCreateUniverseFromSpecError(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/customers/cust1/universes/clusters", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid userIntent"})
	})
	if _, err := f.client().CreateUniverseFromSpec(context.Background(), sampleUniverseSpec()); err == nil {
		t.Fatalf("expected create error")
	}
}

// TestWaitUniverseReady polls until the universe stops updating, then confirms
// the last update succeeded.
func TestWaitUniverseReady(t *testing.T) {
	f := newFake()
	defer f.close()
	var polls int
	f.on("GET /api/v1/customers/cust1/universes", func(w http.ResponseWriter, _ *http.Request) {
		polls++
		inProgress := polls < 2 // ready on the 2nd poll
		writeJSON(w, http.StatusOK, []any{map[string]any{
			"universeUUID": "u-1", "name": "mekoqa-run42",
			"universeDetails": map[string]any{"updateInProgress": inProgress, "updateSucceeded": true},
		}})
	})
	if err := f.client().WaitUniverseReady(context.Background(), "u-1", time.Minute); err != nil {
		t.Fatalf("wait ready: %v", err)
	}
	if polls < 2 {
		t.Errorf("did not poll until ready (polls=%d)", polls)
	}
}

// TestWaitUniverseReadyFailsOnUnsuccessfulUpdate: update finished but did not
// succeed → error (a half-built backing store must not pass).
func TestWaitUniverseReadyFailsOnUnsuccessfulUpdate(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/universes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{map[string]any{
			"universeUUID": "u-1", "name": "mekoqa-run42",
			"universeDetails": map[string]any{"updateInProgress": false, "updateSucceeded": false},
		}})
	})
	if err := f.client().WaitUniverseReady(context.Background(), "u-1", time.Minute); err == nil {
		t.Fatalf("expected error when updateSucceeded=false")
	}
}

// TestCreateUniverseFromSpecRequiresProvider rejects a spec with no provider.
func TestCreateUniverseFromSpecRequiresProvider(t *testing.T) {
	f := newFake()
	defer f.close()
	s := sampleUniverseSpec()
	s.ProviderUUID = ""
	if _, err := f.client().CreateUniverseFromSpec(context.Background(), s); err == nil {
		t.Fatalf("expected error for missing provider uuid")
	}
}

// TestGFlagsFromConfigMergesDefaults lets config override or extend the default
// flags, and treats an empty value as "drop this default".
func TestGFlagsFromConfigMergesDefaults(t *testing.T) {
	defaults := map[string]string{"ysql_enable_auto_analyze": "true", "ysql_pg_conf_csv": "yb_enable_cbo=on"}
	g := gflagsFromConfig(fakeConfig{m: map[string]any{
		"apps.yba.universe.tserver_gflags": map[string]any{
			"ysql_pg_conf_csv":         "yb_enable_cbo=on,log_statement=all",
			"ysql_enable_auto_analyze": "",
			"ysql_log_statement":       "ddl",
		},
	}}, "apps.yba.universe.tserver_gflags", defaults)

	if g["ysql_pg_conf_csv"] != "yb_enable_cbo=on,log_statement=all" {
		t.Errorf("config override lost: ysql_pg_conf_csv = %q", g["ysql_pg_conf_csv"])
	}
	if g["ysql_log_statement"] != "ddl" {
		t.Errorf("config extension lost: ysql_log_statement = %q", g["ysql_log_statement"])
	}
	if _, ok := g["ysql_enable_auto_analyze"]; ok {
		t.Errorf("empty value did not drop the default: %v", g)
	}

	if def := gflagsFromConfig(fakeConfig{}, "apps.yba.universe.tserver_gflags", defaults); def["ysql_enable_auto_analyze"] != "true" {
		t.Errorf("unconfigured run lost the default flags: %v", def)
	}
}

// TestBuildConfigureRequestGFlags asserts the spec's gflags reach the userIntent
// on every provider.
func TestBuildConfigureRequestGFlags(t *testing.T) {
	for _, providerCode := range []string{"aws", "gcp", "kubernetes"} {
		t.Run(providerCode, func(t *testing.T) {
			s := sampleUniverseSpec()
			s.ProviderCode = providerCode
			s.TServerGFlags = map[string]string{"ysql_enable_auto_analyze": "true"}
			s.MasterGFlags = map[string]string{"master_flag": "1"}
			ui := BuildConfigureRequest(s)["clusters"].([]any)[0].(map[string]any)["userIntent"].(map[string]any)

			ts, _ := ui["tserverGFlags"].(map[string]any)
			if ts["ysql_enable_auto_analyze"] != "true" {
				t.Errorf("tserverGFlags = %v", ui["tserverGFlags"])
			}
			ms, _ := ui["masterGFlags"].(map[string]any)
			if ms["master_flag"] != "1" {
				t.Errorf("masterGFlags = %v", ui["masterGFlags"])
			}
		})
	}
}

// TestBuildConfigureRequestNoGFlagsOmitsKeys keeps a spec with no gflags sending
// exactly the request it sent before gflags existed. An empty map is not the
// same request as an absent key, and YBA reads the difference.
func TestBuildConfigureRequestNoGFlagsOmitsKeys(t *testing.T) {
	s := sampleUniverseSpec()
	s.MasterGFlags, s.TServerGFlags = nil, nil
	ui := BuildConfigureRequest(s)["clusters"].([]any)[0].(map[string]any)["userIntent"].(map[string]any)
	for _, k := range []string{"masterGFlags", "tserverGFlags"} {
		if _, ok := ui[k]; ok {
			t.Errorf("%s present with no gflags configured: %v", k, ui[k])
		}
	}
}

// TestBuildUniverseSpecCarriesConfiguredGFlags: every provider's spec builder
// picks the configured tserver flags up.
func TestBuildUniverseSpecCarriesConfiguredGFlags(t *testing.T) {
	c := fakeConfig{m: map[string]any{
		"versions.yugabytedb":              "2025.2.1.0-b141",
		"apps.yba.universe.tserver_gflags": map[string]any{"ysql_enable_auto_analyze": "true"},
	}}
	for name, s := range map[string]UniverseSpec{
		"aws": BuildUniverseSpec(c),
		"gcp": BuildGCPUniverseSpec(c),
		"k8s": BuildK8sUniverseSpec(c),
	} {
		if s.TServerGFlags["ysql_enable_auto_analyze"] != "true" {
			t.Errorf("%s: TServerGFlags = %v", name, s.TServerGFlags)
		}
	}
}

func TestBuildConfigureRequestK8sPodsAreNotEvictable(t *testing.T) {
	ui := BuildConfigureRequest(sampleK8sUniverseSpec())["clusters"].([]any)[0].(map[string]any)["userIntent"].(map[string]any)
	raw, ok := ui["universeOverrides"].(string)
	if !ok {
		t.Fatalf("universeOverrides = %#v, want a YAML string", ui["universeOverrides"])
	}
	var v map[string]map[string]map[string]string
	if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("universeOverrides is not YAML the chart can read: %v", err)
	}
	for _, role := range []string{"master", "tserver"} {
		if got := v[role]["podAnnotations"]["cluster-autoscaler.kubernetes.io/safe-to-evict"]; got != "false" {
			t.Errorf("%s.podAnnotations safe-to-evict = %q, want \"false\"", role, got)
		}
	}
}

func TestBuildConfigureRequestVMUniversesCarryNoK8sOverrides(t *testing.T) {
	ui := BuildConfigureRequest(sampleUniverseSpec())["clusters"].([]any)[0].(map[string]any)["userIntent"].(map[string]any)
	if _, ok := ui["universeOverrides"]; ok {
		t.Errorf("a VM universe carries universeOverrides: %v", ui["universeOverrides"])
	}
}
