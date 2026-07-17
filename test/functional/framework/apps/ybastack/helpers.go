package ybastack

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Object/label names ybastack owns in a namespace. The managed-by label lets
// Teardown refuse a namespace it did not create (mirrors ampstack).
const (
	ManagedByLabel = "ampqa-managed-by"
	ManagedByValue = "ybastack"
)

// namespaceFor renders the tagged, reapable namespace name for a run:
// "<prefix>-<runTag>". The prefix defaults to DefaultNamespacePrefix.
func namespaceFor(prefix, runTag string) string {
	if prefix == "" {
		prefix = DefaultNamespacePrefix
	}
	return prefix + "-" + runTag
}

// shortVersion is the first three dot components of a version string
// ("2025.2.1.0-b141" → "2025.2.1"), matching the yugaware chart filename the
// stress framework builds (task_create_k8s_yugaware.install_yugaware).
func shortVersion(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) <= 3 {
		return version
	}
	return strings.Join(parts[:3], ".")
}

// chartS3URL builds the S3 URL of the yugaware helm chart tarball for a version,
// mirroring the stress recipe: the directory is the full version, the tarball
// filename carries the short (three-component) version.
func chartS3URL(version string) string {
	return fmt.Sprintf("s3://releases.yugabyte.com/%s/helm/yugaware-%s.tgz", version, shortVersion(version))
}

// helmReleaseFor derives the helm release name from the namespace when none is
// configured, mirroring the stress installation_name ("<namespace>-yugaware").
func helmReleaseFor(release, namespace string) string {
	if release != "" {
		return release
	}
	return namespace + "-yugaware"
}

var (
	repositoryRe = regexp.MustCompile(`repository: [\w./-]+`)
	pullSecretRe = regexp.MustCompile(`pullSecret: [\w.-]+`)
)

// patchValuesYAML rewrites the chart's values.yaml image `repository:` to the
// configured registry and `pullSecret:` to the in-cluster secret name — the
// same textual substitution the stress installer (unzip_chart) performs, kept
// as a string patch so it never has to model the whole values schema.
func patchValuesYAML(content, imageRegistry, pullSecretName string) string {
	if imageRegistry != "" {
		content = repositoryRe.ReplaceAllString(content, "repository: "+imageRegistry)
	}
	if pullSecretName != "" {
		content = pullSecretRe.ReplaceAllString(content, "pullSecret: "+pullSecretName)
	}
	return content
}

// helmEscape escapes the characters helm's --set/--set-string grammar treats
// specially inside a value (the backslash escape itself and the comma
// assignment-separator), so a value carrying a comma survives as one literal
// instead of being split into bogus assignments. Mirrors ampstack.helmEscape.
func helmEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, ",", `\,`)
	return s
}

// registryHost returns the registry host component of an image ref
// ("quay.io/yugabyte/yugaware-itest" → "quay.io"), or the whole string when it
// has no slash.
func registryHost(imageRegistry string) string {
	if i := strings.IndexByte(imageRegistry, '/'); i >= 0 {
		return imageRegistry[:i]
	}
	return imageRegistry
}

// suffixClusterRoleBindings suffixes every ClusterRoleBinding's metadata.name in
// a (possibly multi-doc) manifest with "-<namespace>", returning the rewritten
// manifest and the new names. ClusterRoleBindings are CLUSTER-scoped with fixed
// chart names, so two parallel YBAs would clobber each other's binding; the
// per-namespace suffix isolates them (the isolation guarantee — mirrors the
// stress installer) and the returned names are what Teardown must delete, since
// deleting the namespace never removes a cluster-scoped object.
func suffixClusterRoleBindings(manifest, namespace string) (string, []string, error) {
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	var out strings.Builder
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	var names []string
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("decode rbac doc: %w", err)
		}
		if doc == nil {
			continue
		}
		if kindStr(doc["kind"]) == "ClusterRoleBinding" {
			if meta, ok := doc["metadata"].(map[string]any); ok {
				if name := kindStr(meta["name"]); name != "" {
					newName := name + "-" + namespace
					meta["name"] = newName
					names = append(names, newName)
				}
			}
		}
		if err := enc.Encode(doc); err != nil {
			return "", nil, fmt.Errorf("re-encode rbac doc: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return "", nil, fmt.Errorf("close rbac encoder: %w", err)
	}
	return out.String(), names, nil
}

// replaceRBACPlaceholders fills the universe-management RBAC template placeholders
// the stress installer fills: the SA namespace and the service-account name, plus
// the commented namespace line the chart ships. content is one or more YAML docs.
func replaceRBACPlaceholders(content, namespace, serviceAccount string) string {
	content = strings.ReplaceAll(content, "<SA_NAMESPACE>", namespace)
	content = strings.ReplaceAll(content, "yugabyte-platform-universe-management", serviceAccount)
	content = regexp.MustCompile(`# namespace: [\w-]+`).ReplaceAllString(content, "namespace: "+namespace)
	return content
}

// quayDockerConfigJSONB64 builds the base64 .dockerconfigjson body for the quay
// image-pull Secret from a resolved secret value. Two shapes are supported: a
// value that is already a dockerconfigjson (starts with '{' and contains
// "auths") is base64-encoded verbatim; otherwise the value is treated as the
// pre-encoded registry credential (the docker `auth` field) for registryHost.
func quayDockerConfigJSONB64(secretValue, host string) (string, error) {
	v := strings.TrimSpace(secretValue)
	if v == "" {
		return "", fmt.Errorf("quay pull secret value is empty")
	}
	if strings.HasPrefix(v, "{") && strings.Contains(v, "auths") {
		var probe map[string]any
		if err := json.Unmarshal([]byte(v), &probe); err != nil {
			return "", fmt.Errorf("quay pull secret looks like dockerconfigjson but is invalid JSON: %w", err)
		}
		return base64.StdEncoding.EncodeToString([]byte(v)), nil
	}
	if host == "" {
		host = "quay.io"
	}
	cfg := map[string]any{
		"auths": map[string]any{
			host: map[string]any{"auth": v},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// pullSecretManifest builds a dockerconfigjson image-pull Secret manifest from a
// pre-encoded .dockerconfigjson value, labelled ampqa-managed so it is reaped
// with the namespace. Mirrors ampstack.pullSecretManifest.
func pullSecretManifest(namespace, name, dockerConfigB64 string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
  labels:
    ampqa: "1"
    %s: %s
type: kubernetes.io/dockerconfigjson
data:
  .dockerconfigjson: %s
`, name, namespace, ManagedByLabel, ManagedByValue, dockerConfigB64)
}

// serviceAccountManifest builds the yw-sa ServiceAccount manifest (labelled
// ampqa-managed).
func serviceAccountManifest(namespace, name string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: %s
  namespace: %s
  labels:
    ampqa: "1"
    %s: %s
`, name, namespace, ManagedByLabel, ManagedByValue)
}

// namespaceLabels are stamped on the namespace ybastack creates, so Teardown
// (and any reaper) can recognize and safely reclaim it.
func namespaceLabels(extra map[string]string) map[string]string {
	out := map[string]string{
		"ampqa":        "1",
		ManagedByLabel: ManagedByValue,
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// labelPath escapes dots in a label key for a kubectl jsonpath segment.
func labelPath(key string) string { return strings.ReplaceAll(key, ".", `\.`) }

// sortedSetStringArgs renders a stable, sorted "--set-string k=v" argv from a
// map (each value helm-escaped), so the helm install command is deterministic.
func sortedSetStringArgs(kv map[string]string) []string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	// small map: insertion sort keeps output stable without importing sort.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	out := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		out = append(out, "--set-string", k+"="+helmEscape(kv[k]))
	}
	return out
}

func kindStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
