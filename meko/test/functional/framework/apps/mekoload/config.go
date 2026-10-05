// Package mekoload runs meko-load, the HTTP-controlled load client from
// yugabyte/qa-services, in pods next to a stack. The coordinator only steers it:
// it fetches the binary, copies it into the pods, creates a target that points
// at the stack's in-cluster Services, starts runs, polls their summaries and
// stops them. The load itself never leaves the cluster.
package mekoload

import (
	"errors"
	"fmt"
	"time"
)

// Getter is the typed-config surface FromConfig reads. *core.Config satisfies it,
// and this package does not import core, like every other apps/* package.
type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetDuration(key string, def time.Duration) time.Duration
}

// Config says where the binary comes from and how its pods look.
type Config struct {
	// Binary is a ready linux binary on the coordinator. It wins over Source and
	// the release.
	Binary string
	// Source is a meko-load checkout, built with CGO_ENABLED=0 GOOS=linux. It wins
	// over the release.
	Source string
	// Repo and ReleaseTag pick a qa-services release. An empty tag takes the newest
	// one whose tag starts with TagPrefix. Token authenticates the GitHub API.
	Repo       string
	ReleaseTag string
	TagPrefix  string
	Token      string
	// APIBase is the GitHub API root, overridable for tests.
	APIBase  string
	CacheDir string

	Arch   string
	Image  string
	Port   int
	CPU    string
	Memory string

	ReadyTimeout time.Duration
	// Reach is how the coordinator reaches a pod's control API: "tailnet" dials the
	// pod IP and falls back to a port-forward, "port_forward" forwards straight away.
	Reach string
}

// FromConfig reads apps.mekoload.*. token is the resolved GitHub token, which the
// caller reads from the secret named by apps.mekoload.github_secret.
func FromConfig(get Getter, token string) (Config, error) {
	c := Config{
		Binary:       get.GetString("apps.mekoload.binary", ""),
		Source:       get.GetString("apps.mekoload.source", ""),
		Repo:         get.GetString("apps.mekoload.release.repo", "yugabyte/qa-services"),
		ReleaseTag:   get.GetString("apps.mekoload.release.tag", ""),
		TagPrefix:    get.GetString("apps.mekoload.release.tag_prefix", "meko-load-"),
		Token:        token,
		APIBase:      get.GetString("apps.mekoload.release.api_base", "https://api.github.com"),
		CacheDir:     get.GetString("apps.mekoload.cache_dir", ""),
		Arch:         get.GetString("apps.mekoload.arch", "amd64"),
		Image:        get.GetString("apps.mekoload.image", "mirror.gcr.io/library/alpine:3.20"),
		Port:         get.GetInt("apps.mekoload.port", 7080),
		CPU:          get.GetString("apps.mekoload.cpu", "1"),
		Memory:       get.GetString("apps.mekoload.memory", "512Mi"),
		ReadyTimeout: get.GetDuration("apps.mekoload.ready_timeout", 3*time.Minute),
		Reach:        get.GetString("env.mekostack.reach", "tailnet"),
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	switch c.Arch {
	case "amd64", "arm64":
	default:
		return fmt.Errorf("apps.mekoload.arch %q must be amd64 or arm64", c.Arch)
	}
	if c.Image == "" {
		return errors.New("apps.mekoload.image is empty")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("apps.mekoload.port %d is not a port", c.Port)
	}
	return nil
}

// Source describes where Resolve found the binary, for the log and the report.
type Origin struct {
	Kind string // binary, source or release
	Ref  string // the path, the directory or the release tag
	Path string
}
