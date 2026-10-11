// Package mockpod puts the mekoqa binary into the pods of the utility apps
// (`mekoqa mock llm`, `mekoqa mock cognito`). The pods run a public image with
// a shell and tar, and the binary is copied in once the pod is up, the way
// meko-load reaches its pods.
package mockpod

import (
	"bufio"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

const (
	DefaultImage = "mirror.gcr.io/library/alpine:3.20"
	DefaultArch  = "amd64"

	BinDir    = "/opt/mekoqa"
	BinPath   = BinDir + "/mekoqa"
	ReadyFile = BinDir + "/.ready"
	VolName   = "mekoqa-bin"

	// BinLabel carries the binary's digest on the pod, so a re-deploy with a new
	// binary rolls the pod and Install finds the pod of the right generation.
	BinLabel = "mekoqa/bin"
	// ConfigLabel does the same for the mounted config, which can change while the
	// binary does not.
	ConfigLabel = "mekoqa/config"

	modulePath = "github.com/yugabyte/meko/meko/test/functional"
	mainPkg    = "./framework/cmd/mekoqa"
)

type Config struct {
	// Image needs a shell and tar, nothing else.
	Image  string
	Arch   string
	Binary string
}

func (c Config) WithDefaults() Config {
	if c.Image == "" {
		c.Image = DefaultImage
	}
	if c.Arch == "" {
		c.Arch = DefaultArch
	}
	return c
}

func (c Config) Validate() error {
	switch c.Arch {
	case "amd64", "arm64":
		return nil
	}
	return fmt.Errorf("env.mekostack.mocks.arch %q must be amd64 or arm64", c.Arch)
}

type Binary struct {
	Path string
	SHA  string
}

func (b Binary) Label() string {
	if len(b.SHA) > 16 {
		return b.SHA[:16]
	}
	return b.SHA
}

var (
	mu    sync.Mutex
	known = map[string]Binary{}
)

// Resolve returns a linux binary of mekoqa for c.Arch. A cross-compile happens
// at most once per process.
func Resolve(ctx context.Context, c Config) (Binary, error) {
	c = c.WithDefaults()
	if err := c.Validate(); err != nil {
		return Binary{}, err
	}
	mu.Lock()
	defer mu.Unlock()
	key := c.Arch + "|" + c.Binary
	if b, ok := known[key]; ok {
		return b, nil
	}
	path, err := locate(ctx, c)
	if err != nil {
		return Binary{}, err
	}
	sum, err := digest(path)
	if err != nil {
		return Binary{}, err
	}
	b := Binary{Path: path, SHA: sum}
	known[key] = b
	return b, nil
}

func locate(ctx context.Context, c Config) (string, error) {
	if c.Binary != "" {
		st, err := os.Stat(c.Binary)
		if err != nil || st.IsDir() {
			return "", fmt.Errorf("env.mekostack.mocks.binary %q is not a file: %v", c.Binary, err)
		}
		return c.Binary, nil
	}
	if exe, err := os.Executable(); err == nil && runtime.GOOS == "linux" && runtime.GOARCH == c.Arch && staticELF(exe) {
		return exe, nil
	}
	return crossBuild(ctx, c.Arch)
}

// staticELF reports whether path runs on a musl image: a glibc-linked binary
// asks for an interpreter the alpine image does not have.
func staticELF(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return false
		}
	}
	return true
}

func crossBuild(ctx context.Context, arch string) (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", fmt.Errorf("cross-compile mekoqa for linux/%s: %w (set env.mekostack.mocks.binary instead)", arch, err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	dir := filepath.Join(cache, "mekoqa", "mocks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(dir, "mekoqa-linux-"+arch)
	tmp := fmt.Sprintf("%s.%d", out, os.Getpid())
	defer os.Remove(tmp)
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-s -w", "-o", tmp, mainPkg)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cross-compile mekoqa for linux/%s: %w: %s", arch, err, strings.TrimSpace(string(b)))
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	return out, nil
}

func moduleRoot() (string, error) {
	var starts []string
	if _, file, _, ok := runtime.Caller(0); ok {
		starts = append(starts, filepath.Dir(file))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	for _, start := range starts {
		for d := start; ; d = filepath.Dir(d) {
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

func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func Command(args ...string) []string {
	wait := fmt.Sprintf(`while [ ! -f %s ]; do sleep 1; done; exec "$@"`, ReadyFile)
	return append([]string{"sh", "-c", wait, "sh", BinPath}, args...)
}

// PodAnnotations keeps the cluster autoscaler from moving a mock: the binary is
// copied into the pod once, so a rescheduled pod waits for it forever.
func PodAnnotations() map[string]any {
	return map[string]any{"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"}
}

func Volume() map[string]any {
	return map[string]any{"name": VolName, "emptyDir": map[string]any{}}
}

func VolumeMount() map[string]any {
	return map[string]any{"name": VolName, "mountPath": BinDir}
}

func Install(ctx context.Context, kube *k8s.Kubectl, app string, b Binary, config string, timeout time.Duration) error {
	selector := fmt.Sprintf("app=%s,%s=%s", app, BinLabel, b.Label())
	if config != "" {
		selector += fmt.Sprintf(",%s=%s", ConfigLabel, config)
	}
	pod, err := waitRunning(ctx, kube, selector, timeout)
	if err != nil {
		return fmt.Errorf("%s: no running pod for the current binary: %w", app, err)
	}
	ex := kube.ForPod(pod, "")
	if res, err := ex.Run(ctx, "test -f "+ReadyFile); err == nil && res.ExitCode == 0 {
		return nil
	}
	if err := ex.Copy(ctx, b.Path, BinPath); err != nil {
		return fmt.Errorf("copy mekoqa into %s: %w", pod, err)
	}
	res, err := ex.Run(ctx, fmt.Sprintf("chmod 755 %s && touch %s", BinPath, ReadyFile))
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("release mekoqa in %s: %v %s", pod, err, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func waitRunning(ctx context.Context, kube *k8s.Kubectl, selector string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	const running = `{.items[?(@.status.phase=="Running")].metadata.name}`
	for {
		out, err := kube.GetSelector(ctx, "pod", selector, running)
		if err == nil {
			if names := strings.Fields(out); len(names) > 0 {
				return names[0], nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("none Running within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
