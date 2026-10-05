package mekoload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const binaryName = "meko-load"

// Fetcher resolves the binary to copy into the pods.
type Fetcher struct {
	cfg  Config
	hc   *http.Client
	logf func(string, ...any)
	// build compiles the module in dir into out, and is replaced in tests.
	build func(ctx context.Context, dir, out, arch string) error
}

func NewFetcher(cfg Config, logf func(string, ...any)) *Fetcher {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Fetcher{cfg: cfg, hc: &http.Client{Timeout: 5 * time.Minute}, logf: logf, build: goBuild}
}

// Resolve returns a linux binary. An explicit path wins, then a source checkout,
// then the newest release. The first two need nothing from the network, and
// the release is what a Jenkins agent without the qa-services checkout uses.
func (f *Fetcher) Resolve(ctx context.Context) (Origin, error) {
	switch {
	case f.cfg.Binary != "":
		st, err := os.Stat(f.cfg.Binary)
		if err != nil || st.IsDir() {
			return Origin{}, fmt.Errorf("apps.mekoload.binary %q is not a file: %v", f.cfg.Binary, err)
		}
		return Origin{Kind: "binary", Ref: f.cfg.Binary, Path: f.cfg.Binary}, nil
	case f.cfg.Source != "":
		out, err := f.cacheFile("source", binaryName+"-"+f.cfg.Arch)
		if err != nil {
			return Origin{}, err
		}
		f.logf("mekoload: building %s for linux/%s", f.cfg.Source, f.cfg.Arch)
		if err := f.build(ctx, f.cfg.Source, out, f.cfg.Arch); err != nil {
			return Origin{}, fmt.Errorf("build meko-load from %s: %w", f.cfg.Source, err)
		}
		return Origin{Kind: "source", Ref: f.cfg.Source, Path: out}, nil
	}
	return f.fromRelease(ctx)
}

func goBuild(ctx context.Context, dir, out, arch string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/meko-load")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

func (f *Fetcher) cacheFile(sub, name string) (string, error) {
	root := f.cfg.CacheDir
	if root == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		root = filepath.Join(base, "mekoqa", "meko-load")
	}
	dir := filepath.Join(root, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

type release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

func (f *Fetcher) fromRelease(ctx context.Context) (Origin, error) {
	if f.cfg.Token == "" {
		return Origin{}, fmt.Errorf("no meko-load binary: set apps.mekoload.binary or apps.mekoload.source, or give the GitHub token the release download needs (secrets.%s)", "github")
	}
	rel, err := f.pickRelease(ctx)
	if err != nil {
		return Origin{}, err
	}
	want := fmt.Sprintf("meko-load_linux_%s", f.cfg.Arch)
	bin, ok := findAsset(rel, want)
	if !ok {
		return Origin{}, fmt.Errorf("release %s has no asset %s", rel.TagName, want)
	}
	out, err := f.cacheFile(rel.TagName, want)
	if err != nil {
		return Origin{}, err
	}
	sums := ""
	if a, ok := findAsset(rel, "checksums.txt"); ok {
		b, err := f.download(ctx, a)
		if err != nil {
			return Origin{}, fmt.Errorf("download checksums.txt: %w", err)
		}
		sums = string(b)
	}
	if st, err := os.Stat(out); err == nil && st.Size() == bin.Size && verifySum(out, want, sums) == nil {
		f.logf("mekoload: using the cached %s of %s", want, rel.TagName)
		return Origin{Kind: "release", Ref: rel.TagName, Path: out}, nil
	}
	f.logf("mekoload: downloading %s of %s", want, rel.TagName)
	b, err := f.download(ctx, bin)
	if err != nil {
		return Origin{}, fmt.Errorf("download %s: %w", want, err)
	}
	tmp := out + ".part"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return Origin{}, err
	}
	if err := verifySum(tmp, want, sums); err != nil {
		_ = os.Remove(tmp)
		return Origin{}, err
	}
	if err := os.Rename(tmp, out); err != nil {
		return Origin{}, err
	}
	return Origin{Kind: "release", Ref: rel.TagName, Path: out}, nil
}

func (f *Fetcher) pickRelease(ctx context.Context) (release, error) {
	repo := f.cfg.Repo
	if tag := f.cfg.ReleaseTag; tag != "" {
		var r release
		if err := f.api(ctx, fmt.Sprintf("/repos/%s/releases/tags/%s", repo, url.PathEscape(tag)), &r); err != nil {
			return release{}, fmt.Errorf("release %s: %w", tag, err)
		}
		return r, nil
	}
	var all []release
	if err := f.api(ctx, fmt.Sprintf("/repos/%s/releases?per_page=100", repo), &all); err != nil {
		return release{}, fmt.Errorf("list releases of %s: %w", repo, err)
	}
	for _, r := range all {
		if !r.Draft && !r.Prerelease && strings.HasPrefix(r.TagName, f.cfg.TagPrefix) {
			return r, nil
		}
	}
	return release{}, fmt.Errorf("%s has no release tagged %s*", repo, f.cfg.TagPrefix)
}

func (f *Fetcher) api(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.cfg.APIBase, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+f.cfg.Token)
	resp, err := f.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	return json.Unmarshal(body, out)
}

func (f *Fetcher) download(ctx context.Context, a asset) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+f.cfg.Token)
	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %d for asset %s", resp.StatusCode, a.Name)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func findAsset(r release, name string) (asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return asset{}, false
}

// verifySum checks the file against its line in a sha256sum listing. With no
// listing there is nothing to check.
func verifySum(path, name, listing string) error {
	if listing == "" {
		return nil
	}
	var want string
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			want = fields[0]
		}
	}
	if want == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s has sha256 %s, checksums.txt says %s", name, got, want)
	}
	return nil
}
