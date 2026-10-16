package mekostack

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type DataPlaneSecrets struct {
	DBPassword           string
	MCPInternal          string
	LangfuseNextauth     string
	LangfuseSalt         string
	LangfuseEncryptKey   string
	LangfuseAdminAPIKey  string
	LangfuseInitEmail    string
	LangfuseInitPassword string
	LangfuseProjectKey   string
	// CognitoClientSecret is the Cognito mock's app client secret. It never
	// goes in the values file.
	CognitoClientSecret string
}

func NewDataPlaneSecrets(r io.Reader) (DataPlaneSecrets, error) {
	if r == nil {
		r = rand.Reader
	}
	var err error
	h := func(n int) string {
		b := make([]byte, n)
		if _, e := io.ReadFull(r, b); e != nil && err == nil {
			err = e
		}
		return hex.EncodeToString(b)
	}
	s := DataPlaneSecrets{
		DBPassword:           h(16),
		MCPInternal:          h(16),
		LangfuseNextauth:     h(16),
		LangfuseSalt:         h(16),
		LangfuseEncryptKey:   h(32),
		LangfuseAdminAPIKey:  h(16),
		LangfuseInitEmail:    "mekoqa@mekoqa.test",
		LangfuseInitPassword: h(16),
		LangfuseProjectKey:   "sk-lf-" + h(16),
		CognitoClientSecret:  h(16),
	}
	if err != nil {
		return DataPlaneSecrets{}, fmt.Errorf("generate stack secrets: %w", err)
	}
	return s, nil
}

// MinIO is the S3 the stack uses: the api-server reaches it by its Service name,
// and the rag workers by the Service ClusterIP.
//
// The workers read uploads with a bare boto3 client, and boto3 addresses a
// bucket as a subdomain of a custom endpoint unless the host is an IP address.
// Nothing in the chart can pass them an AWS config file that says otherwise.
type MinIO struct {
	AccessKey string
	SecretKey string
	// APIEndpoint is for the api-server (Go SDK, path-style forced).
	APIEndpoint string
	// WorkerEndpoint is for the rag workers.
	WorkerEndpoint string
}

// Values builds the generated values file, laid over the cloud, env and mode
// presets. It sets what differs per stack and nothing the presets already say.
func (cfg Config) Values(s DataPlaneSecrets, m MinIO) (map[string]any, error) {
	dbRepo, dbTag, err := SplitRef(cfg.DatabaseImage)
	if err != nil {
		return nil, fmt.Errorf("env.mekostack.images.database: %w", err)
	}
	tag := func(key string) map[string]any { return map[string]any{"tag": cfg.Tag(key)} }

	worker := func() map[string]any {
		return map[string]any{"image": tag("rag_agent"), "nodeSelector": nil}
	}
	apiConfig := map[string]any{"S3_BUCKET": cfg.S3Bucket, "S3_REGION": cfg.S3Region, "S3_ENDPOINT": m.APIEndpoint}
	if cfg.S3PublicEndpoint != "" {
		apiConfig["S3_PUBLIC_ENDPOINT"] = cfg.S3PublicEndpoint
	}

	secrets := map[string]any{
		"dbPassword":                   s.DBPassword,
		"mcpInternalSecret":            s.MCPInternal,
		"openaiApiKey":                 cfg.OpenAIKey,
		"s3AccessKey":                  m.AccessKey,
		"s3SecretKey":                  m.SecretKey,
		"langfuseNextauthSecret":       s.LangfuseNextauth,
		"langfuseSalt":                 s.LangfuseSalt,
		"langfuseEncryptionKey":        s.LangfuseEncryptKey,
		"langfuseAdminApiKey":          s.LangfuseAdminAPIKey,
		"langfuseInitUserEmail":        s.LangfuseInitEmail,
		"langfuseInitUserPassword":     s.LangfuseInitPassword,
		"langfuseInitProjectSecretKey": s.LangfuseProjectKey,
		"langfuseS3AccessKey":          m.AccessKey,
		"langfuseS3SecretKey":          m.SecretKey,
	}
	if cfg.RagWorkers {
		secrets["ragAwsAccessKeyId"] = m.AccessKey
		secrets["ragAwsSecretAccessKey"] = m.SecretKey
	}
	v := map[string]any{
		"dataPlaneSecrets": secrets,
		"yugabyte": map[string]any{
			"Image": map[string]any{"repository": dbRepo, "tag": dbTag},
		},
		"apiServer": map[string]any{
			"replicas":   cfg.APIReplicas,
			"image":      tag("api_server"),
			"migrate":    map[string]any{"extensionsImage": cfg.DatabaseImage},
			"staticArgs": map[string]any{"s3AuthMode": "minio"},
			"config":     apiConfig,
		},
		"mekoMcpServer": map[string]any{"image": tag("mcp_server")},
		"mekoUi":        map[string]any{"image": tag("ui")},
		"inferenceGateway": map[string]any{
			"image": tag("inference_gateway"),
			"config": map[string]any{
				"INFERENCE_LLM_PROVIDER":       "openai",
				"INFERENCE_LLM_MODEL":          "gpt-4o-mini",
				"INFERENCE_CONV_SUMMARY_MODEL": "",
			},
		},
		"langfuse": map[string]any{
			"web":    map[string]any{"image": tag("langfuse_web")},
			"worker": map[string]any{"image": tag("langfuse_worker")},
			"config": langfuseS3(cfg, m),
		},
	}
	if cfg.RagWorkers {
		ragConfig := map[string]any{
			"AWS_ENDPOINT_URL_S3": m.WorkerEndpoint,
			"AWS_S3_BUCKET_NAME":  cfg.S3Bucket,
			"AWS_REGION":          cfg.S3Region,
		}
		rag := map[string]any{
			"enabled":       true,
			"gpu":           map[string]any{"enabled": false},
			"text":          worker(),
			"conversations": worker(),
			"pdf":           worker(),
			"config":        ragConfig,
		}
		if cfg.MockLLM() {
			for k, val := range cfg.llmEnv() {
				ragConfig[k] = val
			}
			rag["openai"] = map[string]any{"enabled": true}
		}
		v["ragAgent"] = rag
	} else {
		v["ragAgent"] = map[string]any{"enabled": false}
	}
	if cfg.Backing != nil && !cfg.Backing.bundled() {
		cfg.Backing.valuesFor(v, cfg.dbPassword(s), cfg.RagWorkers)
	}
	if cfg.HashPartitions > 0 {
		migrate := v["apiServer"].(map[string]any)["migrate"].(map[string]any)
		migrate["hashPartitions"] = strconv.Itoa(cfg.HashPartitions)
	}
	if cfg.MCPReplicas > 0 {
		v["mekoMcpServer"].(map[string]any)["replicas"] = cfg.MCPReplicas
	}
	if cfg.MockLLM() {
		env := cfg.llmEnv()
		gw := v["inferenceGateway"].(map[string]any)["config"].(map[string]any)
		for k, val := range env {
			gw[k] = val
		}
		v["apiServer"].(map[string]any)["extraEnv"] = cfg.apiLLMEnv()
		mcp := v["mekoMcpServer"].(map[string]any)
		mcp["extraEnv"] = cfg.mcpLLMEnv()
		// extraEnv carries AWS_REGION, and server-side apply refuses a second one from the chart.
		mcp["staticEnv"] = map[string]any{"awsRegion": ""}
	}
	// The chart passes only the config keys it names, so a feature flag goes in as env.
	if len(cfg.APIConfig) > 0 {
		api := v["apiServer"].(map[string]any)
		env, _ := api["extraEnv"].([]any)
		for _, kv := range cfg.APIConfig {
			k, val, _ := strings.Cut(kv, "=")
			env = append(env, map[string]any{"name": k, "value": val})
		}
		api["extraEnv"] = env
	}
	return v, nil
}

// MarshalValues renders a values tree as YAML. yaml.v3 quotes a string that would
// read back as another type, which matters for the all-digit hex a random key can
// be: unquoted it becomes a number, and the chart sees an empty secret.
func MarshalValues(v map[string]any) ([]byte, error) {
	return yaml.Marshal(v)
}

// langfuseS3 points Langfuse's event, media and export uploads at the stack's
// MinIO. Without a store Langfuse rejects every OTLP trace, so no conversation
// is ever recorded.
func langfuseS3(cfg Config, m MinIO) map[string]any {
	out := map[string]any{}
	for _, kind := range []string{"EVENT_UPLOAD", "MEDIA_UPLOAD", "BATCH_EXPORT"} {
		p := "LANGFUSE_S3_" + kind + "_"
		out[p+"BUCKET"] = cfg.S3Bucket
		out[p+"REGION"] = cfg.S3Region
		out[p+"ENDPOINT"] = m.APIEndpoint
		out[p+"FORCE_PATH_STYLE"] = "true"
	}
	return out
}

var secretValueKeys = map[string]struct{}{
	"dbPassword": {}, "mcpInternalSecret": {}, "openaiApiKey": {}, "s3AccessKey": {}, "s3SecretKey": {},
	"langfuseNextauthSecret": {}, "langfuseSalt": {}, "langfuseEncryptionKey": {}, "langfuseAdminApiKey": {},
	"langfuseInitUserPassword": {}, "langfuseInitProjectSecretKey": {}, "ragAwsAccessKeyId": {}, "ragAwsSecretAccessKey": {},
	"langfuseS3AccessKey": {}, "langfuseS3SecretKey": {},
}

func MaskedValues(v map[string]any) map[string]any {
	return maskTree(v, false).(map[string]any)
}

func maskTree(node any, inSecrets bool) any {
	switch t := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if inSecrets {
				if _, secret := secretValueKeys[k]; secret {
					out[k] = "********"
					continue
				}
			}
			if k == "dbConnectionString" {
				out[k] = "********"
				continue
			}
			out[k] = maskTree(val, inSecrets || k == "dataPlaneSecrets")
		}
		return out
	default:
		return node
	}
}

func SecretLiterals(v map[string]any) []string {
	var out []string
	var walk func(node any, inSecrets bool)
	walk = func(node any, inSecrets bool) {
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		for k, val := range m {
			if s, isStr := val.(string); isStr && inSecrets && s != "" {
				if _, secret := secretValueKeys[k]; secret {
					out = append(out, s)
				}
			}
			walk(val, inSecrets || k == "dataPlaneSecrets")
		}
	}
	walk(v, false)
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func RedactArgs(line string, literals []string) string {
	for _, l := range literals {
		line = strings.ReplaceAll(line, l, "********")
	}
	return line
}
