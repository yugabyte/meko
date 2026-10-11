package mekostack

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mockpod"
)

const modulePath = "github.com/yugabyte/meko/meko/test/functional"

// ReachMode is how the coordinator reaches the api-server and mcp-server.
type ReachMode string

const (
	// ReachTailnet dials the Service ClusterIP, then the pod IP, and falls back to
	// a kubectl port-forward when neither answers. Both addresses are routable
	// from a coordinator on the tailnet, and a forward is a local process that
	// can die mid-run, so it is the last resort.
	ReachTailnet ReachMode = "tailnet"
	// ReachPortForward forwards straight away, without probing.
	ReachPortForward ReachMode = "port_forward"
)

const (
	LLMMock = "mock"
	LLMOff  = "off"
)

// Getter is the typed-config surface FromConfig reads. *core.Config satisfies it,
// and this package does not import core, like every other apps/* package.
type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetBool(key string, def bool) bool
	GetDuration(key string, def time.Duration) time.Duration
	GetStringSlice(key string, def []string) []string
}

// Config describes one stack.
type Config struct {
	Context   string
	Namespace string
	// RunID, Owner and TTL stamp the namespace so a janitor can tell whose it is
	// and when it may go.
	RunID string
	Owner string
	TTL   time.Duration

	// ChartPath is the chart directory, ValuesFiles the presets inside it in
	// order, last wins.
	ChartPath   string
	Release     string
	ValuesFiles []string

	// ImageVersion is the tag of every meko component, ImageTags overrides it per
	// component key, and DatabaseImage is a full reference.
	ImageVersion  string
	ImageTags     map[string]string
	DatabaseImage string

	APIReplicas int
	// MCPReplicas, when set, overrides the values files' meko-mcp-server count.
	MCPReplicas int
	// HashPartitions, when set, is the partition count of each hash-partitioned
	// data table the stack's migrations create. 0 keeps the chart's 128.
	HashPartitions int
	// APIConfig is extra api-server env, KEY=VALUE, for runtime feature flags the
	// presets leave off.
	APIConfig []string
	// Auth is who the api-server believes, and CognitoMock how the mock pool is
	// built when Auth is cognito_mock.
	Auth        AuthMode
	CognitoMock cognitomock.Spec
	RagWorkers  bool
	// LLMOff leaves every caller on the real providers.
	LLM string
	// Mocks is how the pods of the LLM and Cognito mocks get the mekoqa binary.
	Mocks mockpod.Config
	// The gateway exits at startup without an OpenAI key, and the mock accepts any.
	OpenAIKey string

	QuayUser  string
	QuayToken string

	MinIOServerImage string
	MinIOClientImage string

	S3Bucket         string
	S3Region         string
	S3PublicEndpoint string

	ProvisionTimeout time.Duration
	ReadyTimeout     time.Duration
	RolloutTimeout   time.Duration

	// Backing is the database the stack uses instead of the chart's own. Nil
	// leaves the bundled one.
	Backing *Backing

	Reach ReachMode
	// ValuesDir and DiagDir are where the generated values file and the failure
	// diagnostics land. Empty ValuesDir means the system temp dir, and empty
	// DiagDir means nothing is written to disk.
	ValuesDir string
	DiagDir   string
}

// FromConfig reads env.mekostack.* from get. Namespace, run id, owner, context
// and credentials come from the caller, which owns those.
func FromConfig(get Getter) (Config, error) {
	chart, err := ResolveChart(get.GetString("env.mekostack.chart", ""))
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ChartPath:      chart,
		Release:        get.GetString("env.mekostack.release", ""),
		ValuesFiles:    get.GetStringSlice("env.mekostack.values_files", nil),
		ImageVersion:   get.GetString("env.mekostack.image_version", ""),
		ImageTags:      map[string]string{},
		DatabaseImage:  get.GetString("env.mekostack.images.database", ""),
		APIReplicas:    get.GetInt("env.mekostack.api_replicas", 0),
		MCPReplicas:    get.GetInt("env.mekostack.mcp_replicas", 0),
		APIConfig:      get.GetStringSlice("env.mekostack.api_config", nil),
		HashPartitions: get.GetInt("env.mekostack.hash_partitions", 0),
		Auth:           AuthMode(get.GetString("env.mekostack.auth", string(AuthCognitoMock))),
		CognitoMock: cognitomock.Spec{
			Region:   get.GetString("env.mekostack.cognito_mock.region", ""),
			PoolID:   get.GetString("env.mekostack.cognito_mock.pool_id", ""),
			ClientID: get.GetString("env.mekostack.cognito_mock.client_id", ""),
			TokenTTL: get.GetDuration("env.mekostack.cognito_mock.token_ttl", 0),
		},
		RagWorkers: get.GetBool("env.mekostack.rag_workers", false),
		LLM:        get.GetString("env.mekostack.llm", LLMMock),
		Mocks: mockpod.Config{
			Image:  get.GetString("env.mekostack.mocks.image", ""),
			Arch:   get.GetString("env.mekostack.mocks.arch", ""),
			Binary: get.GetString("env.mekostack.mocks.binary", ""),
		},
		OpenAIKey:        get.GetString("env.mekostack.openai_api_key", ""),
		MinIOServerImage: get.GetString("env.mekostack.minio.server_image", ""),
		MinIOClientImage: get.GetString("env.mekostack.minio.client_image", ""),
		S3Bucket:         get.GetString("env.mekostack.s3.bucket", ""),
		S3Region:         get.GetString("env.mekostack.s3.region", ""),
		S3PublicEndpoint: get.GetString("env.mekostack.s3.public_endpoint", ""),
		ProvisionTimeout: get.GetDuration("env.mekostack.timeouts.provision", 0),
		ReadyTimeout:     get.GetDuration("env.mekostack.timeouts.ready", 0),
		RolloutTimeout:   get.GetDuration("env.mekostack.timeouts.rollout", 0),
		Reach:            ReachMode(get.GetString("env.mekostack.reach", string(ReachTailnet))),
		ValuesDir:        get.GetString("env.mekostack.values_dir", ""),
		DiagDir:          get.GetString("env.mekostack.diag_dir", ""),
		TTL:              get.GetDuration("env.mekostack.ttl", 0),
	}
	for _, c := range Components {
		if t := get.GetString("env.mekostack.images."+c.Key+".tag", ""); t != "" {
			cfg.ImageTags[c.Key] = t
		}
	}
	return cfg, nil
}

func (cfg Config) MockLLM() bool { return cfg.LLM == LLMMock }

// Validate checks everything that can be known before a cluster is touched. A
// failure is a usage error, so it must run before anything is created.
func (cfg Config) Validate() error {
	var errs []error
	if cfg.Release == "" {
		errs = append(errs, errors.New("env.mekostack.release is empty"))
	}
	if len(cfg.ValuesFiles) == 0 {
		errs = append(errs, errors.New("env.mekostack.values_files is empty"))
	}
	var refs []string
	for _, c := range Components {
		tag := cfg.Tag(c.Key)
		switch {
		case tag == "":
			errs = append(errs, fmt.Errorf("no tag for %s: set env.mekostack.image_version or env.mekostack.images.%s.tag", c.Repo, c.Key))
		case !ValidTag(tag):
			errs = append(errs, fmt.Errorf("env.mekostack.images.%s.tag %q is not an image tag", c.Key, tag))
		default:
			refs = append(refs, c.Ref(tag))
		}
	}
	if _, tag, err := SplitRef(cfg.DatabaseImage); err != nil {
		errs = append(errs, fmt.Errorf("env.mekostack.images.database: %w", err))
	} else if !ValidTag(tag) {
		errs = append(errs, fmt.Errorf("env.mekostack.images.database tag %q is not an image tag", tag))
	} else {
		refs = append(refs, cfg.DatabaseImage)
	}
	if err := CheckQuayOnly(refs); err != nil {
		errs = append(errs, err)
	}
	for _, kv := range cfg.APIConfig {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			errs = append(errs, fmt.Errorf("env.mekostack.api_config entry %q is not KEY=VALUE", kv))
		}
	}
	if cfg.APIReplicas < 1 {
		errs = append(errs, fmt.Errorf("env.mekostack.api_replicas = %d, want at least 1", cfg.APIReplicas))
	}
	if cfg.HashPartitions < 0 || cfg.HashPartitions > 1000 {
		errs = append(errs, fmt.Errorf("env.mekostack.hash_partitions = %d, want 0 to 1000", cfg.HashPartitions))
	}
	if cfg.MCPReplicas < 0 {
		errs = append(errs, fmt.Errorf("env.mekostack.mcp_replicas = %d, want 0 or more", cfg.MCPReplicas))
	}
	switch cfg.Auth {
	case "", AuthNone, AuthCognitoMock:
	default:
		errs = append(errs, fmt.Errorf("env.mekostack.auth = %q, want %s or %s", cfg.Auth, AuthNone, AuthCognitoMock))
	}
	if err := cfg.Mocks.WithDefaults().Validate(); err != nil {
		errs = append(errs, err)
	}
	if cfg.S3Bucket == "" {
		errs = append(errs, errors.New("env.mekostack.s3.bucket is empty"))
	}
	if cfg.OpenAIKey == "" {
		errs = append(errs, errors.New("env.mekostack.openai_api_key is empty: the inference gateway exits at startup without one"))
	}
	switch cfg.LLM {
	case LLMMock, LLMOff:
	default:
		errs = append(errs, fmt.Errorf("env.mekostack.llm = %q, want %s or %s", cfg.LLM, LLMMock, LLMOff))
	}
	if cfg.QuayUser == "" || cfg.QuayToken == "" {
		errs = append(errs, errors.New("no Quay credential: every meko image and the database image are private, so declare secrets.quay as \"<user>:<token>\" (or set env.mekostack.quay_user and quay_token)"))
	}
	if cfg.Backing != nil && !cfg.Backing.bundled() {
		if err := cfg.Backing.validate(); err != nil {
			errs = append(errs, err)
		}
	}
	switch cfg.Reach {
	case ReachTailnet, ReachPortForward:
	default:
		errs = append(errs, fmt.Errorf("env.mekostack.reach = %q, want %s or %s", cfg.Reach, ReachTailnet, ReachPortForward))
	}
	if st, err := os.Stat(filepath.Join(cfg.ChartPath, "Chart.yaml")); err != nil || st.IsDir() {
		errs = append(errs, fmt.Errorf("no chart at %s (env.mekostack.chart)", cfg.ChartPath))
	}
	return errors.Join(errs...)
}

// ResolveChart makes a configured chart path absolute. A relative path is
// resolved against the module root, so the default works from any directory.
func ResolveChart(path string) (string, error) {
	if path == "" {
		return "", errors.New("env.mekostack.chart is empty")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	root, err := ModuleRoot()
	if err != nil {
		return "", fmt.Errorf("resolve chart %q: %w", path, err)
	}
	return filepath.Clean(filepath.Join(root, path)), nil
}

// ModuleRoot finds the directory holding this module's go.mod: where the source
// was built, else above the working directory, else above the binary.
func ModuleRoot() (string, error) {
	var starts []string
	if _, file, _, ok := runtime.Caller(0); ok {
		starts = append(starts, filepath.Dir(file))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, dir := range starts {
		for d := dir; ; d = filepath.Dir(d) {
			if isModuleRoot(d) {
				return d, nil
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return "", fmt.Errorf("no go.mod for module %s above %v", modulePath, starts)
}

func isModuleRoot(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")) == modulePath
		}
	}
	return false
}
