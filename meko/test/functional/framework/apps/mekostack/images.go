package mekostack

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// QuayNamespace is the only registry path a meko image may come from. Releases
// and custom builds of meko-build publish there, and nothing else is a build the
// process knows how to reproduce.
const QuayNamespace = "quay.io/yugabyte"

type Component struct {
	Key  string
	Repo string
}

var Components = []Component{
	{"api_server", "meko-api-server"},
	{"mcp_server", "meko-mcp-server"},
	{"ui", "meko-ui"},
	{"inference_gateway", "meko-inference-gateway"},
	{"rag_agent", "meko-rag-agent"},
	{"langfuse_web", "meko-langfuse-web"},
	{"langfuse_worker", "meko-langfuse-worker"},
}

var tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

func ValidTag(tag string) bool { return tagRe.MatchString(tag) }

func (c Component) Ref(tag string) string { return QuayNamespace + "/" + c.Repo + ":" + tag }

func (cfg Config) Tag(key string) string {
	if t := cfg.ImageTags[key]; t != "" {
		return t
	}
	return cfg.ImageVersion
}

// SplitRef splits repo:tag. A reference with no tag, or with a digest, is an
// error: the chart insists on a tag and the check below reads the repository.
func SplitRef(ref string) (repo, tag string, err error) {
	if strings.Contains(ref, "@") {
		return "", "", fmt.Errorf("image %q: digests are not supported, name a tag", ref)
	}
	i := strings.LastIndexByte(ref, ':')
	if i < 0 || i < strings.LastIndexByte(ref, '/') || i == len(ref)-1 {
		return "", "", fmt.Errorf("image %q has no tag", ref)
	}
	return ref[:i], ref[i+1:], nil
}

// IsMekoImage reports whether ref names one of meko's own images: a repository
// called meko or meko-*, or one under a meko path segment such as meko/rag-agent.
func IsMekoImage(ref string) bool {
	name := ref
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name = name[:i]
	}
	parts := strings.Split(name, "/")
	last := parts[len(parts)-1]
	if last == "meko" || strings.HasPrefix(last, "meko-") {
		return true
	}
	for _, p := range parts[:len(parts)-1] {
		if p == "meko" {
			return true
		}
	}
	return false
}

// CheckQuayOnly refuses any meko image outside quay.io/yugabyte/. Third-party
// public images (valkey, nginx, minio) pass.
func CheckQuayOnly(refs []string) error {
	var bad []string
	for _, ref := range refs {
		if IsMekoImage(ref) && !strings.HasPrefix(ref, QuayNamespace+"/") {
			bad = append(bad, ref)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("meko images must come from %s/ (published by meko-build), not: %s", QuayNamespace, strings.Join(bad, ", "))
}

func RenderedImages(manifest string) ([]string, error) {
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	seen := map[string]struct{}{}
	for {
		var doc any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode rendered manifest: %w", err)
		}
		collectImages(doc, seen)
	}
	out := make([]string, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out, nil
}

func collectImages(node any, into map[string]struct{}) {
	switch t := node.(type) {
	case map[string]any:
		for k, v := range t {
			if s, ok := v.(string); ok && k == "image" && s != "" {
				into[s] = struct{}{}
				continue
			}
			collectImages(v, into)
		}
	case []any:
		for _, e := range t {
			collectImages(e, into)
		}
	}
}

// ChartSupport is what the chart can take as values. A chart that predates them
// gets the same settings applied after install instead.
type ChartSupport struct {
	S3EndpointEnv bool // the api-server Deployment reads S3_ENDPOINT
	RagAWSKeys    bool // rag-agent-secret carries AWS_ACCESS_KEY_ID
	ExtraEnv      bool // the api-server and mcp-server Deployments take extraEnv
	// APIServerExtras is whether the api-server pod carries the host aliases
	// the probe set. Only meaningful for a probe that sets them.
	APIServerExtras bool
}

func RenderedSupport(manifest string) (ChartSupport, error) {
	var cs ChartSupport
	var apiEnv, mcpEnv bool
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				cs.ExtraEnv = apiEnv && mcpEnv
				return cs, nil
			}
			return cs, fmt.Errorf("decode rendered manifest: %w", err)
		}
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		switch {
		case doc["kind"] == "Deployment" && name == apiDeployment:
			cs.S3EndpointEnv = cs.S3EndpointEnv || hasEnv(doc, "S3_ENDPOINT")
			apiEnv = apiEnv || hasEnv(doc, llmProbeEnv)
			cs.APIServerExtras = cs.APIServerExtras || hasHostAliases(doc)
		case doc["kind"] == "Deployment" && name == mcpDeployment:
			mcpEnv = mcpEnv || hasEnv(doc, llmProbeEnv)
		case doc["kind"] == "Secret" && name == ragSecretName:
			data, _ := doc["stringData"].(map[string]any)
			_, cs.RagAWSKeys = data["AWS_ACCESS_KEY_ID"]
		}
	}
}

func hasEnv(node any, name string) bool {
	switch t := node.(type) {
	case map[string]any:
		if env, ok := t["env"].([]any); ok {
			for _, e := range env {
				if m, ok := e.(map[string]any); ok && m["name"] == name {
					return true
				}
			}
		}
		for _, v := range t {
			if hasEnv(v, name) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if hasEnv(e, name) {
				return true
			}
		}
	}
	return false
}

func hasHostAliases(doc map[string]any) bool {
	spec, _ := doc["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	pod, _ := tmpl["spec"].(map[string]any)
	aliases, _ := pod["hostAliases"].([]any)
	return len(aliases) > 0
}
