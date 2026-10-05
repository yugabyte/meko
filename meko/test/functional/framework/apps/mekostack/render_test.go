package mekostack_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

// TestGeneratedValuesRenderWithTheRealChart renders the shipped chart with the
// shipped defaults and the generated values, offline, and checks every meko image
// in the result is on quay.io/yugabyte. It needs helm on PATH and skips without it.
func TestGeneratedValuesRenderWithTheRealChart(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on PATH")
	}
	cfgFile, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mekostack.FromConfig(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ChartPath, "Chart.yaml")); err != nil {
		t.Skipf("no chart at %s", cfg.ChartPath)
	}
	cfg.Namespace = "mekoqa-st-render"
	cfg.QuayUser, cfg.QuayToken = "u", "t"

	for name, mutate := range map[string]func(*mekostack.Config){
		"defaults":        func(*mekostack.Config) {},
		"component tag":   func(c *mekostack.Config) { c.ImageTags["mcp_server"] = "custom-0123456789ab" },
		"without rag":     func(c *mekostack.Config) { c.RagWorkers = false },
		"one api replica": func(c *mekostack.Config) { c.APIReplicas = 1 },
		"llm off":         func(c *mekostack.Config) { c.LLM = mekostack.LLMOff },
		"database by ref": func(c *mekostack.Config) { c.DatabaseImage = "quay.io/yugabyte/meko-yugabytedb:2026.1.0.0-b1-meko-3" },
		"on a backing": func(c *mekostack.Config) {
			c.Backing = &mekostack.Backing{Mode: "yb_k8s", Host: "yb-tservers.mekoqa-yb-render.svc.cluster.local.", Port: 5433}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := cfg
			c.ImageTags = map[string]string{}
			for k, v := range cfg.ImageTags {
				c.ImageTags[k] = v
			}
			mutate(&c)
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			dp, err := mekostack.NewDataPlaneSecrets(nil)
			if err != nil {
				t.Fatal(err)
			}
			values, err := c.Values(dp, mekostack.MinIO{
				AccessKey: "ak", SecretKey: "sk",
				APIEndpoint: "http://minio.ns.svc.cluster.local:9000", WorkerEndpoint: "http://10.96.0.5:9000",
			})
			if err != nil {
				t.Fatal(err)
			}
			b, err := mekostack.MarshalValues(values)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"template", c.Release, c.ChartPath, "-n", c.Namespace}
			for _, f := range c.ValuesFiles {
				args = append(args, "-f", filepath.Join(c.ChartPath, f))
			}
			args = append(args, "-f", path)
			var stdout, stderr bytes.Buffer
			cmd := exec.Command(helm, args...)
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("helm template: %v\n%s", err, stderr.String())
			}
			images, err := mekostack.RenderedImages(stdout.String())
			if err != nil {
				t.Fatal(err)
			}
			if err := mekostack.CheckQuayOnly(images); err != nil {
				t.Fatal(err)
			}
			var meko int
			for _, img := range images {
				if mekostack.IsMekoImage(img) {
					meko++
				}
			}
			want := 8
			if !c.RagWorkers {
				want = 7
			}
			if meko != want {
				t.Errorf("rendered %d distinct meko images, want %d: %v", meko, want, images)
			}
			if name == "defaults" {
				t.Logf("rendered images:\n  %s", strings.Join(images, "\n  "))
			}
			if !strings.Contains(stdout.String(), "AWS_ENDPOINT_URL_S3: \"http://10.96.0.5:9000\"") == c.RagWorkers {
				t.Errorf("rag worker S3 endpoint presence does not match RagWorkers=%v", c.RagWorkers)
			}
			if c.Backing != nil {
				manifest := stdout.String()
				for _, want := range []string{
					`DB_HOST: "yb-tservers.mekoqa-yb-render.svc.cluster.local."`,
					`LANGFUSE_DB_HOST: "yb-tservers.mekoqa-yb-render.svc.cluster.local."`,
					"@yb-tservers.mekoqa-yb-render.svc.cluster.local.:5433/langfuse",
					"@yb-tservers.mekoqa-yb-render.svc.cluster.local.:5433/yugabyte",
				} {
					if !strings.Contains(manifest, want) {
						t.Errorf("the chart rendered on a backing lacks %q", want)
					}
				}
				for _, bundled := range []string{"name: yb-master", "name: yb-tserver"} {
					if strings.Contains(manifest, bundled) {
						t.Errorf("the chart still renders the bundled database (%s)", bundled)
					}
				}
			}
			support, err := mekostack.RenderedSupport(stdout.String())
			if err != nil {
				t.Fatal(err)
			}
			if support.ExtraEnv != c.MockLLM() {
				t.Errorf("the chart takes extraEnv for the LLM env = %v, want %v", support.ExtraEnv, c.MockLLM())
			}
			// api-server, mcp-server, gateway config, rag config.
			wantEnv := 0
			if c.MockLLM() {
				wantEnv = 3
				if c.RagWorkers {
					wantEnv = 4
				}
			}
			if got := strings.Count(stdout.String(), "OPENAI_BASE_URL"); got < wantEnv || (wantEnv == 0 && got != 0) {
				t.Errorf("OPENAI_BASE_URL appears %d times in the render, want at least %d", got, wantEnv)
			}
			if !strings.Contains(stdout.String(), "name: rag-agent-secret") == c.RagWorkers {
				t.Errorf("rag-agent-secret presence does not match RagWorkers=%v", c.RagWorkers)
			}
		})
	}
}

func TestShippedDefaultsAreValid(t *testing.T) {
	cfgFile, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mekostack.FromConfig(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.QuayUser, cfg.QuayToken = "u", "t"
	if _, err := os.Stat(filepath.Join(cfg.ChartPath, "Chart.yaml")); err != nil {
		t.Skipf("no chart at %s", cfg.ChartPath)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the shipped defaults do not validate: %v", err)
	}
	if !filepath.IsAbs(cfg.ChartPath) || !strings.HasSuffix(cfg.ChartPath, filepath.Join("meko", "deploy", "helm", "meko")) {
		t.Errorf("ChartPath = %q, want the repo chart resolved to an absolute path", cfg.ChartPath)
	}
}
