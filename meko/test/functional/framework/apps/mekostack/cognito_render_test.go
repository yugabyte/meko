package mekostack

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

// The overlay has to mean something to the real chart: the Cognito flags on the
// api-server, the host alias and the CA only on the serving container.
func TestCognitoValuesRenderWithTheRealChart(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on PATH")
	}
	cfgFile, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := FromConfig(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ChartPath, "Chart.yaml")); err != nil {
		t.Skipf("no chart at %s", cfg.ChartPath)
	}
	cfg.Namespace, cfg.QuayUser, cfg.QuayToken = "mekoqa-st-render", "u", "t"
	cfg.Auth = AuthCognitoMock
	dp, err := NewDataPlaneSecrets(nil)
	if err != nil {
		t.Fatal(err)
	}
	values, err := cfg.Values(dp, MinIO{AccessKey: "ak", SecretKey: "sk", APIEndpoint: "http://minio:9000", WorkerEndpoint: "http://10.0.0.1:9000"})
	if err != nil {
		t.Fatal(err)
	}
	values = overlay(values, apiServerCognito(cfg.CognitoMock.WithDefaults(), cognitoMockHost{ClusterIP: "10.9.8.7"}, true))
	b, err := MarshalValues(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"template", cfg.Release, cfg.ChartPath, "-n", cfg.Namespace}
	for _, f := range cfg.ValuesFiles {
		args = append(args, "-f", filepath.Join(cfg.ChartPath, f))
	}
	args = append(args, "-f", path)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(helm, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"--cognito-region=$(COGNITO_REGION)", "COGNITO_POOL_ID: \"us-east-1_MekoQa001\"",
		"ip: 10.9.8.7", "cognito-idp.us-east-1.amazonaws.com", "name: SSL_CERT_FILE", "secretName: cognito-mock",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the render lacks %q", want)
		}
	}
	cs, err := RenderedSupport(out)
	if err != nil || !cs.APIServerExtras {
		t.Errorf("support = %+v %v, want the chart to be seen taking the host alias", cs, err)
	}
}
