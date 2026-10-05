package mekostack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

const (
	llmProbeEnv   = "OPENAI_BASE_URL"
	mcpDeployment = "meko-mcp-server"
	mcpContainer  = "meko-mcp"

	// Signing a Bedrock request needs a key pair, and the mock accepts any.
	mockAWSKeyID  = "AKIAMEKOQAMOCK"
	mockAWSSecret = "mekoqa-mock-secret"
	mockAWSRegion = "us-east-1"
)

func (cfg Config) llmEnv() map[string]string {
	return map[string]string{
		"OPENAI_BASE_URL":                  llmmock.OpenAIBaseURL(llmmock.DefaultName, cfg.Namespace),
		"AWS_ENDPOINT_URL_BEDROCK_RUNTIME": llmmock.EndpointFor(llmmock.DefaultName, cfg.Namespace),
	}
}

func envList(m map[string]string, order ...string) []any {
	out := make([]any, 0, len(order))
	for _, k := range order {
		out = append(out, map[string]any{"name": k, "value": m[k]})
	}
	return out
}

func (cfg Config) apiLLMEnv() []any {
	return envList(cfg.llmEnv(), "OPENAI_BASE_URL")
}

// The mcp-server embeds with Titan when the knowledge-base index was made for Bedrock.
func (cfg Config) mcpLLMEnv() []any {
	env := cfg.llmEnv()
	env["AWS_ACCESS_KEY_ID"] = mockAWSKeyID
	env["AWS_SECRET_ACCESS_KEY"] = mockAWSSecret
	env["AWS_REGION"] = mockAWSRegion
	return envList(env, "OPENAI_BASE_URL", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_REGION")
}

func (a *App) setupLLM(ctx context.Context) error {
	if !a.cfg.MockLLM() {
		return nil
	}
	if _, err := a.deployLLM(ctx, a.kube, llmmock.Spec{Pod: a.cfg.Mocks, ReadyTimeout: a.cfg.ReadyTimeout}); err != nil {
		return err
	}
	return nil
}

func deployLLMMock(ctx context.Context, kube *k8s.Kubectl, spec llmmock.Spec) (*llmmock.Server, error) {
	return llmmock.Deploy(ctx, kube, spec)
}

// patchLLMEnv is for a chart that has no extraEnv value.
func (a *App) patchLLMEnv(ctx context.Context) error {
	for _, t := range []struct {
		deployment, container string
		env                   []any
	}{
		{apiDeployment, apiDeployment, a.cfg.apiLLMEnv()},
		{mcpDeployment, mcpContainer, a.cfg.mcpLLMEnv()},
	} {
		patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": t.container, "env": t.env}},
		}}}})
		if err != nil {
			return err
		}
		if err := a.kube.Patch(ctx, "deployment", t.deployment, "strategic", string(patch)); err != nil {
			return fmt.Errorf("point %s at the LLM mock: %w", t.deployment, err)
		}
	}
	for _, d := range []string{apiDeployment, mcpDeployment} {
		if err := a.kube.RolloutStatus(ctx, "deployment", d, a.cfg.RolloutTimeout); err != nil {
			return fmt.Errorf("%s rollout after the LLM env change: %w", d, err)
		}
	}
	return nil
}

// A stack built without a mock has no Service, and LLM() then answers nil.
func (a *App) connectLLM(ctx context.Context) error {
	if !a.cfg.MockLLM() {
		return nil
	}
	if _, err := a.kube.Get(ctx, "service", llmmock.DefaultName, "{.metadata.name}"); err != nil {
		if k8s.IsNotFound(err) {
			a.log("mekostack: no %s Service in %s, this stack was built without the LLM mock", llmmock.DefaultName, a.cfg.Namespace)
			return nil
		}
		return fmt.Errorf("look for the LLM mock: %w", err)
	}
	url, err := a.reach(ctx, llmmock.DefaultName, llmmock.Port, "app="+llmmock.DefaultName, llmmock.Port)
	if err != nil {
		return fmt.Errorf("reach the LLM mock: %w", err)
	}
	a.mu.Lock()
	a.llmURL = url
	a.mu.Unlock()
	return nil
}

func (a *App) LLMURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.llmURL
}

func (a *App) LLM() *llmmock.Client {
	if u := a.LLMURL(); u != "" {
		return llmmock.NewClient(u)
	}
	return nil
}

var errNoLLM = errors.New("this stack has no LLM mock")

func (a *App) llmCalls(ctx context.Context) ([]byte, error) {
	c := a.LLM()
	if c == nil {
		return nil, errNoLLM
	}
	calls, err := c.Calls(ctx, llmmock.Filter{})
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(calls, "", "  ")
}
