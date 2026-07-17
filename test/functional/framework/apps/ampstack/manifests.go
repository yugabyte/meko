package ampstack

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Object/label names ampstack owns in a namespace.
const (
	// ManagedByLabel marks a namespace as ampqa-managed; Teardown refuses to
	// delete a namespace lacking it (the label value must be ManagedByValue).
	ManagedByLabel = "ampqa-managed-by"
	ManagedByValue = "ampstack"

	yugabytedStatefulSet = "yugabyted"
	yugabytedPod         = "yugabyted-0"

	shimConfigMapName = "ampqa-helm-shim"
	shimMountPath     = "/ampqa-bin"
	// shimPATH prepends the shim dir so the controller's bare `helm` resolves to
	// the wrapper. The tail mirrors a standard container PATH.
	shimPATH = shimMountPath + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// yugabytedManifest is a single-node yugabyted StatefulSet + headless Service,
// mirroring deploy/up.sh step 3 (parameterized by namespace, image, storage
// size, and — when non-empty — a storage class for the data PVC). The
// `$(hostname -f)` expressions are evaluated by the container's bash at runtime
// and must stay literal in the YAML.
func yugabytedManifest(namespace, image, storage, storageClass string) string {
	sc := ""
	if storageClass != "" {
		sc = fmt.Sprintf("\n        storageClassName: %s", storageClass)
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: yugabyted
  namespace: %[1]s
  labels: { app: yugabyted }
spec:
  clusterIP: None
  publishNotReadyAddresses: true
  selector: { app: yugabyted }
  ports:
    - { name: ysql, port: 5433, targetPort: 5433 }
    - { name: master-ui, port: 7000, targetPort: 7000 }
    - { name: tserver-ui, port: 9000, targetPort: 9000 }
    - { name: yugabyted-ui, port: 15433, targetPort: 15433 }
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: yugabyted
  namespace: %[1]s
  labels: { app: yugabyted }
spec:
  serviceName: yugabyted
  replicas: 1
  selector:
    matchLabels: { app: yugabyted }
  template:
    metadata:
      labels: { app: yugabyted }
    spec:
      terminationGracePeriodSeconds: 60
      securityContext:
        fsGroup: 1000
      containers:
        - name: yugabyted
          image: %[2]s
          imagePullPolicy: IfNotPresent
          command: ["bash", "-c"]
          args:
            - |
              exec bin/yugabyted start --background=false --base_dir=/data --advertise_address="$(hostname -f)" --tserver_flags=ysql_enable_auth=false
          ports:
            - { name: ysql, containerPort: 5433 }
            - { name: master-ui, containerPort: 7000 }
            - { name: tserver-ui, containerPort: 9000 }
            - { name: yugabyted-ui, containerPort: 15433 }
          volumeMounts:
            - { name: data, mountPath: /data }
          readinessProbe:
            exec:
              command:
                - bash
                - -c
                - 'bin/ysqlsh -h "$(hostname -f)" -p 5433 -U yugabyte -d yugabyte -c "select 1" >/dev/null 2>&1'
            initialDelaySeconds: 20
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 60
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: [ReadWriteOnce]%[4]s
        resources:
          requests:
            storage: %[3]s
`, namespace, image, storage, sc)
}

// createStorageDBScript is the sh -c body run inside yugabyted-0 to ensure the
// yb_storage database exists (deploy/up.sh step 4). yugabyted binds YSQL to its
// advertise address (the pod FQDN), so it connects there, not loopback. It is
// idempotent and tolerates a racy "already exists".
func createStorageDBScript(dbName string) string {
	return fmt.Sprintf(`set -e
H="$(hostname -f)"
exists="$(bin/ysqlsh -h "$H" -p 5433 -U yugabyte -d yugabyte -tAc "SELECT 1 FROM pg_database WHERE datname='%[1]s'")"
if [ "$exists" = "1" ]; then
  echo "%[1]s already exists"
elif out="$(bin/ysqlsh -h "$H" -p 5433 -U yugabyte -d yugabyte -c "CREATE DATABASE %[1]s" 2>&1)"; then
  echo "created %[1]s"
elif printf '%%s' "$out" | grep -q "already exists"; then
  echo "%[1]s already exists"
else
  printf 'CREATE DATABASE %[1]s failed: %%s\n' "$out" >&2
  exit 1
fi`, dbName)
}

// pullSecretManifest builds a dockerconfigjson image-pull Secret from a
// pre-encoded (base64) .dockerconfigjson value.
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

// registryAuth is one registry's credentials in a .dockerconfigjson.
type registryAuth struct {
	User     string
	Password string
}

// dockerConfigJSONB64 builds the base64 .dockerconfigjson body a docker-registry
// Secret carries, from per-registry credentials (ECR: user "AWS" + the
// `aws ecr get-login-password` output; quay: a robot account user+token).
// Mirrors what `kubectl create secret docker-registry` writes
// (deploy/ecr-creds.sh) but supports several registries in one secret, so an
// env pulling the controller from quay and other images from ECR needs only
// the one imagePullSecrets entry the chart already references.
func dockerConfigJSONB64(auths map[string]registryAuth) string {
	entries := map[string]any{}
	for registry, a := range auths {
		entries[registry] = map[string]any{
			"username": a.User,
			"password": a.Password,
			"auth":     base64.StdEncoding.EncodeToString([]byte(a.User + ":" + a.Password)),
		}
	}
	b, _ := json.Marshal(map[string]any{"auths": entries})
	return base64.StdEncoding.EncodeToString(b)
}

// shimScript is the POSIX-sh helm wrapper. The deployed controller image
// (amp/controller:tailnet-mig0010) predates yb-amp commit 2bb15684b, which
// dropped --create-namespace from endpoint helm deploys. On a namespace-scoped
// install the controller lacks cluster-wide namespaces:create, so that flag
// fails every endpoint deploy. This wrapper strips --create-namespace and
// delegates to the real helm, rebuilding the argument list exactly (each arg
// single-quoted, embedded quotes escaped) so nothing else is mangled.
const shimScript = `#!/bin/sh
REAL_HELM=/usr/local/bin/helm
quoted=
for a in "$@"; do
  [ "$a" = "--create-namespace" ] && continue
  esc=$(printf '%s' "$a" | sed "s/'/'\\''/g")
  quoted="$quoted '$esc'"
done
eval "exec \"\$REAL_HELM\"$quoted"
`

// shimConfigMapManifest embeds the helm shim script under the "helm" key.
func shimConfigMapManifest(namespace string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
  labels:
    ampqa: "1"
    %s: %s
data:
  helm: |
`, shimConfigMapName, namespace, ManagedByLabel, ManagedByValue)
	for _, line := range strings.Split(strings.TrimRight(shimScript, "\n"), "\n") {
		b.WriteString("    ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// shimDeploymentPatch is the strategic-merge patch that mounts the shim
// ConfigMap into the controller container (mode 0755) and prepends its dir to
// PATH, so the controller's bare `helm` resolves to the wrapper. Strategic
// merge keys volumes/volumeMounts/env by name, so it merges cleanly onto the
// chart-rendered pod spec. container is the controller container name.
func shimDeploymentPatch(container string) string {
	return fmt.Sprintf(`spec:
  template:
    spec:
      volumes:
        - name: %[1]s
          configMap:
            name: %[1]s
            defaultMode: 0755
      containers:
        - name: %[2]s
          volumeMounts:
            - name: %[1]s
              mountPath: %[3]s
          env:
            - name: PATH
              value: %[4]s
`, shimConfigMapName, container, shimMountPath, shimPATH)
}

// namespaceLabels returns the labels ampstack stamps on a namespace it creates,
// merged with any extras the caller supplies. The managed-by label is always
// present so Teardown can refuse foreign namespaces.
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

// splitImageRef splits a "registry/repo:tag" ref into repository and tag,
// splitting on the last colon only when it follows the last slash (so a
// registry host:port prefix is not mistaken for a tag). An absent tag yields
// ("ref", "").
func splitImageRef(ref string) (repository, tag string) {
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

// sortedSetArgs renders a stable, sorted "--set k=v" argv slice from a map, so
// the helm-template command is deterministic (test-friendly).
func sortedSetArgs(flag string, kv map[string]string) []string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		out = append(out, flag, k+"="+kv[k])
	}
	return out
}

// helmEscape escapes the two characters helm's --set/--set-string grammar
// treats specially inside a value: backslash (the escape character itself)
// and comma (the multi-assignment separator). A value with unescaped commas
// — a CSV like "host1:5433,host2:5433" or a DSN whose password happens to
// carry one — gets split into bogus extra "key=value" fragments instead of
// staying one literal value; helm un-escapes "\," back to "," when it renders
// the chart, so the rendered manifest ends up with the original literal text.
func helmEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, ",", `\,`)
	return s
}
