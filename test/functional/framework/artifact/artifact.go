// Package artifact implements the declarative artifact system (architecture
// §4.7, implementation §3 "Artifacts"): the stress framework's artifacts.conf
// model, in Go. Tests and tasks work with external things — workload apps,
// utilities, datasets, binaries — declared once in config under a versions{}
// block (name→version string) and an artifacts{} block (name→declaration),
// and consumed by name; nothing hand-rolls a download.
package artifact

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// Type selects which fetcher resolves an artifact.
type Type string

const (
	TypeGithubRelease Type = "github_release"
	TypeS3            Type = "s3"
	TypeHTTP          Type = "http"
	TypeLocal         Type = "local"
)

// Spec is one resolved artifact declaration: {key} placeholders substituted
// from versions{}, validated, ready to fetch and deploy.
type Spec struct {
	Name string
	// Version is the cache-path component (see cachePath). When the
	// declaration omits an explicit "version" key, it defaults to the
	// (already-substituted) GlobPath, so distinct versions still land in
	// distinct cache directories keyed by their resolved filename.
	Version  string
	GlobPath string // filename on the target/cache, after substitution
	URL      string // fetch source, after substitution
	Type     Type
	// RemoteDownload: fetch directly on the target (curl/aws-cli there),
	// instead of landing in the local cache first.
	RemoteDownload bool
	// InternalDownload: fetch/upload once, then relay to the remaining
	// targets — see Deployer.deployInternalFanout's package-doc comment for
	// the KISS simplification used here.
	InternalDownload bool
	// RemoteUnzip: after landing (by either delivery path), unpack the
	// tar.gz/tgz/tar/zip on the target.
	RemoteUnzip bool
	// Target is a free-form scope label ("client"/"server"/"all"/...) matched
	// against the label the caller passes to Deploy — the framework never
	// hardcodes roles.
	Target string
	// Secret names a config key under secrets.<name> holding the token/
	// credential used for the fetch. artifact/ never reads secrets.* itself —
	// callers resolve the value and pass it to Deploy, so this package stays
	// agnostic of the secrets config shape.
	Secret string
}

// placeholderRe matches a leftover {...} token after substitution.
var placeholderRe = regexp.MustCompile(`\{[^{}]+\}`)

// Resolve reads the versions{} and artifacts{} config sections, substitutes
// {key} placeholders in glob_path/url/version from versions{}, and validates
// every declaration. Returns a name→Spec map (empty, not an error, when no
// artifacts{} section exists).
func Resolve(cfg *core.Config) (map[string]Spec, error) {
	root := cfg.Resolved()
	versions, err := versionsMap(root["versions"])
	if err != nil {
		return nil, fmt.Errorf("versions: %w", err)
	}
	rawArtifacts, ok := root["artifacts"]
	if !ok || rawArtifacts == nil {
		return map[string]Spec{}, nil
	}
	artifactsMap, ok := rawArtifacts.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("artifacts: expected a map, got %T", rawArtifacts)
	}
	specs := make(map[string]Spec, len(artifactsMap))
	for name, raw := range artifactsMap {
		decl, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("artifact %q: expected a map, got %T", name, raw)
		}
		spec, err := resolveOne(name, decl, versions)
		if err != nil {
			return nil, err
		}
		specs[name] = spec
	}
	return specs, nil
}

func resolveOne(name string, decl map[string]any, versions map[string]string) (Spec, error) {
	globPath := strField(decl, "glob_path")
	if globPath == "" {
		return Spec{}, fmt.Errorf("artifact %q: glob_path is required", name)
	}
	globPath, err := substitute(globPath, versions)
	if err != nil {
		return Spec{}, fmt.Errorf("artifact %q: glob_path: %w", name, err)
	}

	url := strField(decl, "url")
	if url == "" {
		return Spec{}, fmt.Errorf("artifact %q: url is required", name)
	}
	url, err = substitute(url, versions)
	if err != nil {
		return Spec{}, fmt.Errorf("artifact %q: url: %w", name, err)
	}

	typ := Type(strField(decl, "type"))
	switch typ {
	case TypeGithubRelease, TypeS3, TypeHTTP, TypeLocal:
	default:
		return Spec{}, fmt.Errorf("artifact %q: unknown type %q (want github_release/s3/http/local)", name, typ)
	}

	version := strField(decl, "version")
	if version != "" {
		version, err = substitute(version, versions)
		if err != nil {
			return Spec{}, fmt.Errorf("artifact %q: version: %w", name, err)
		}
	} else {
		version = globPath
	}

	return Spec{
		Name:             name,
		Version:          version,
		GlobPath:         globPath,
		URL:              url,
		Type:             typ,
		RemoteDownload:   boolField(decl, "remote_download"),
		InternalDownload: boolField(decl, "internal_download"),
		RemoteUnzip:      boolField(decl, "remote_unzip"),
		Target:           strField(decl, "target"),
		Secret:           strField(decl, "secret"),
	}, nil
}

// substitute replaces every {key} in s with versions[key], then errors if any
// {...} token remains unresolved.
func substitute(s string, versions map[string]string) (string, error) {
	for k, v := range versions {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	if leftover := placeholderRe.FindString(s); leftover != "" {
		return "", fmt.Errorf("unresolved placeholder %s (not present in versions{})", leftover)
	}
	return s, nil
}

func versionsMap(raw any) (map[string]string, error) {
	if raw == nil {
		return map[string]string{}, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a map, got %T", raw)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
			continue
		}
		out[k] = fmt.Sprint(v)
	}
	// Derive a "<key>_base" placeholder (the version with its "-bNNN" build
	// suffix stripped) for every version that carries one, so a single version
	// string can drive both the build-suffixed filename ({name}) and the public
	// release DIRECTORY ({name_base}).
	derived := make(map[string]string, len(out))
	for k, v := range out {
		if base := core.BaseVersion(v); base != "" && base != v {
			derived[k+"_base"] = base
		}
	}
	for k, v := range derived {
		out[k] = v
	}
	return out, nil
}

func strField(decl map[string]any, key string) string {
	v, ok := decl[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func boolField(decl map[string]any, key string) bool {
	v, ok := decl[key]
	if !ok || v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	if s, ok := v.(string); ok {
		b, _ := strconv.ParseBool(s)
		return b
	}
	return false
}
