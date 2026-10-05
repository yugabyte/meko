package deploy

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var voyagerTestMigrationID = uuid.MustParse("90b8b0b8-9013-4602-8898-fba5bf21f564")

func voyagerTestSpec(operation VoyagerOperation) VoyagerRunSpec {
	spec := VoyagerRunSpec{
		MigrationID: voyagerTestMigrationID,
		Operation:   operation,
		Attempt:     2,
		Source: VoyagerSourceConfig{
			DBType:   "postgresql",
			Host:     "source.internal",
			Port:     5432,
			Username: "source_user",
			Password: "source-secret",
			Database: "production",
			Schemas:  []string{"public", "sales"},
		},
		ParallelJobs: 8,
	}
	if operation == VoyagerOperationMigrate || operation == VoyagerOperationEnd {
		spec.Target = &VoyagerTargetConfig{
			Host:     "target.internal",
			Port:     55433,
			Username: "cloud_admin",
			Password: "target-secret",
			Database: "production",
		}
	}
	return spec
}

func TestVoyagerDeterministicNames(t *testing.T) {
	if got, want := voyagerVolumeName(voyagerTestMigrationID), "voyager-exp-90b8b0b8"; got != want {
		t.Fatalf("volume name = %q, want %q", got, want)
	}
	if got, want := voyagerRunName(voyagerTestMigrationID, VoyagerOperationAssess, 1),
		"voyager-assess-90b8b0b8-a1"; got != want {
		t.Fatalf("assess name = %q, want %q", got, want)
	}
	if got, want := voyagerRunName(voyagerTestMigrationID, VoyagerOperationMigrate, 3),
		"voyager-migrate-90b8b0b8-a3"; got != want {
		t.Fatalf("migrate name = %q, want %q", got, want)
	}
	if got, want := voyagerRunName(voyagerTestMigrationID, VoyagerOperationEnd, 1),
		"voyager-end-90b8b0b8-a1"; got != want {
		t.Fatalf("end name = %q, want %q", got, want)
	}

	assess, err := buildVoyagerJobSpec(voyagerTestSpec(VoyagerOperationAssess), "postgres://control")
	if err != nil {
		t.Fatal(err)
	}
	migrate, err := buildVoyagerJobSpec(voyagerTestSpec(VoyagerOperationMigrate), "postgres://control")
	if err != nil {
		t.Fatal(err)
	}
	if assess.Volume != migrate.Volume {
		t.Fatalf("assess volume %q differs from migrate volume %q", assess.Volume, migrate.Volume)
	}
}

func TestVoyagerEnvironmentComposition(t *testing.T) {
	job, err := buildVoyagerJobSpec(voyagerTestSpec(VoyagerOperationMigrate), "postgres://control-secret")
	if err != nil {
		t.Fatal(err)
	}
	env := voyagerEnvMap(job.Env)
	want := map[string]string{
		"CONTROL_PLANE_TYPE":          "yugabyted",
		"YUGABYTED_DB_CONN_STRING":    "postgres://control-secret",
		"YB_VOYAGER_MIGRATION_UUID":   voyagerTestMigrationID.String(),
		"EXPORT_DIR":                  "/voyager/export",
		"YB_VOYAGER_SEND_DIAGNOSTICS": "0",
		"SOURCE_DB_TYPE":              "postgresql",
		"SOURCE_HOST":                 "source.internal",
		"SOURCE_PORT":                 "5432",
		"SOURCE_USER":                 "source_user",
		"SOURCE_DB":                   "production",
		"SOURCE_SCHEMAS":              "public,sales",
		"SOURCE_DB_PASSWORD":          "source-secret",
		"TARGET_HOST":                 "target.internal",
		"TARGET_PORT":                 "55433",
		"TARGET_USER":                 "cloud_admin",
		"TARGET_DB":                   "production",
		"TARGET_DB_PASSWORD":          "target-secret",
		"PARALLEL_JOBS":               "8",
	}
	if len(env) != len(want) {
		t.Fatalf("env has %d entries, want %d", len(env), len(want))
	}
	for name, value := range want {
		if env[name] != value {
			t.Errorf("%s = %q, want %q", name, env[name], value)
		}
	}
	for i := 1; i < len(job.Env); i++ {
		if job.Env[i-1].Name >= job.Env[i].Name {
			t.Fatalf("env is not deterministically sorted at %q, %q", job.Env[i-1].Name, job.Env[i].Name)
		}
	}
}

func TestVoyagerAssessEnvironmentIncludesEmptyTargetFields(t *testing.T) {
	job, err := buildVoyagerJobSpec(voyagerTestSpec(VoyagerOperationAssess), "postgres://control")
	if err != nil {
		t.Fatal(err)
	}
	env := voyagerEnvMap(job.Env)
	for _, name := range []string{
		"TARGET_HOST", "TARGET_PORT", "TARGET_USER", "TARGET_DB", "TARGET_DB_PASSWORD",
	} {
		value, ok := env[name]
		if !ok {
			t.Errorf("%s is absent", name)
		} else if value != "" {
			t.Errorf("%s = %q, want empty for assess", name, value)
		}
	}
}

func TestVoyagerCommandComposition(t *testing.T) {
	assess := assessCommand()
	for _, want := range []string{
		"/opt/amp/voyager/assess.sh",
	} {
		if !strings.Contains(assess, want) {
			t.Errorf("assess command does not contain %q", want)
		}
	}
	if strings.Contains(assess, "yb-voyager assess-migration") {
		t.Error("assess command still invokes yb-voyager assess-migration")
	}

	migrate := migrateCommand()
	if !strings.Contains(migrate, "/opt/amp/voyager/migrate.sh") {
		t.Error("migrate command does not invoke /opt/amp/voyager/migrate.sh")
	}
	if strings.Contains(migrate, "yb-voyager export schema") {
		t.Error("migrate command still inlines the yb-voyager sequence")
	}
	end := endCommand()
	if !strings.Contains(end, "/opt/amp/voyager/end.sh") {
		t.Error("end command does not invoke /opt/amp/voyager/end.sh")
	}
	for _, password := range []string{"source-secret", "target-secret"} {
		if strings.Contains(migrate, password) || strings.Contains(assess, password) || strings.Contains(end, password) {
			t.Errorf("command contains password %q", password)
		}
	}
}

func TestVoyagerEndRequiresTarget(t *testing.T) {
	spec := voyagerTestSpec(VoyagerOperationEnd)
	spec.Target = nil
	if _, err := buildVoyagerJobSpec(spec, "postgres://control"); err == nil ||
		!strings.Contains(err.Error(), "target connection is required for end") {
		t.Fatalf("error = %v, want missing end target error", err)
	}
}

func TestKubernetesJobManifestRuntimePolicy(t *testing.T) {
	runner := NewKubernetesVoyagerJobRunner(KubernetesVoyagerConfig{
		Image:      "registry.example.com/yb-voyager:test",
		Namespace:  "amp",
		PVCSize:    "20Gi",
		PullSecret: "regcred",
	})
	job, err := buildVoyagerJobSpec(voyagerTestSpec(VoyagerOperationMigrate), "postgres://control")
	if err != nil {
		t.Fatal(err)
	}
	manifest := runner.jobManifest(job)
	if manifest.Spec.BackoffLimit == nil || *manifest.Spec.BackoffLimit != 0 {
		t.Fatalf("backoffLimit = %v, want 0", manifest.Spec.BackoffLimit)
	}
	if got := manifest.Spec.Template.Spec.RestartPolicy; got != "Never" {
		t.Fatalf("restartPolicy = %q, want Never", got)
	}
	if got := manifest.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != job.Volume {
		t.Fatalf("claim name = %q, want %q", got, job.Volume)
	}
	if got := manifest.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath; got != voyagerExportDir {
		t.Fatalf("mount path = %q, want %q", got, voyagerExportDir)
	}
	// The non-root image (uid/gid 1000) can only write the freshly provisioned,
	// root-owned PVC if the pod declares a matching fsGroup for Kubernetes to
	// chown the volume to.
	sc := manifest.Spec.Template.Spec.SecurityContext
	if sc == nil {
		t.Fatal("pod securityContext is nil, want fsGroup set for PVC ownership")
	}
	if sc.FSGroup == nil || *sc.FSGroup != voyagerFSGroup {
		t.Fatalf("fsGroup = %v, want %d", sc.FSGroup, voyagerFSGroup)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != voyagerRunAsUser {
		t.Fatalf("runAsUser = %v, want %d", sc.RunAsUser, voyagerRunAsUser)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != voyagerRunAsGroup {
		t.Fatalf("runAsGroup = %v, want %d", sc.RunAsGroup, voyagerRunAsGroup)
	}
}

func TestParseKubernetesJobState(t *testing.T) {
	tests := []struct {
		name string
		body string
		want RunState
	}{
		{name: "pending", body: `{}`, want: RunStatePending},
		{name: "running", body: `{"status":{"active":1}}`, want: RunStateRunning},
		{name: "succeeded count", body: `{"status":{"succeeded":1}}`, want: RunStateSucceeded},
		{name: "complete condition", body: `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`, want: RunStateSucceeded},
		{name: "failed count", body: `{"status":{"failed":1}}`, want: RunStateFailed},
		{name: "failed condition", body: `{"status":{"conditions":[{"type":"Failed","status":"True"}]}}`, want: RunStateFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKubernetesJobState([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("state = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseDockerContainerState(t *testing.T) {
	tests := []struct {
		name string
		body string
		want RunState
	}{
		{name: "created", body: `{"Status":"created","Running":false,"ExitCode":0}`, want: RunStatePending},
		{name: "running", body: `{"Status":"running","Running":true,"ExitCode":0}`, want: RunStateRunning},
		{name: "succeeded", body: `{"Status":"exited","Running":false,"ExitCode":0}`, want: RunStateSucceeded},
		{name: "failed", body: `{"Status":"exited","Running":false,"ExitCode":7}`, want: RunStateFailed},
		{name: "dead", body: `{"Status":"dead","Running":false,"ExitCode":0}`, want: RunStateFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDockerContainerState([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("state = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDockerCommandErrorIncludesOnlyCallerApprovedStderr(t *testing.T) {
	base := errors.New("Docker command failed")
	if got := dockerCommandError(base, " daemon detail \n").Error(); got != "Docker command failed: daemon detail" {
		t.Fatalf("error = %q, want trimmed stderr detail", got)
	}
	if got := dockerCommandError(base, " \n").Error(); got != base.Error() {
		t.Fatalf("error with blank stderr = %q, want %q", got, base)
	}
}

func voyagerEnvMap(vars []voyagerEnvVar) map[string]string {
	env := make(map[string]string, len(vars))
	for _, variable := range vars {
		env[variable.Name] = variable.Value
	}
	return env
}
