package yugabytedk8s

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

const (
	container   = "yugabyted"
	pullSecret  = "quay-registry-creds"
	confMount   = "/conf"
	dataMount   = "/data"
	ysqlPort    = 5433
	labelApp    = "app"
	labelUniv   = "universe"
	labelRole   = "yb-cluster"
	appName     = "yugabyted"
	ysqlsh      = "/home/yugabyte/bin/ysqlsh"
	yugabytedSh = "/home/yugabyte/bin/yugabyted"
)

var nonDNS = regexp.MustCompile(`[^a-z0-9]+`)

// zoneSlug makes a placement key usable in an object name.
func zoneSlug(zone string) string {
	s := strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(universe.ZoneName(zone)), "-"), "-")
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}

func stsName(t universe.ClusterType, zone string) string {
	if t == universe.ReadReplica {
		return "rr-" + zoneSlug(zone)
	}
	return "yb-" + zoneSlug(zone)
}

func (u *Universe) podFQDN(sts string, ordinal int) string {
	return fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local", sts, ordinal, u.spec.Name, u.ns)
}

// firstPod is the pod every other pod joins: the first zone's first pod of the
// primary cluster.
func (u *Universe) firstPod() (sts string, fqdn string) {
	zones := u.spec.Primary().Zones()
	sts = stsName(universe.Primary, zones[0])
	return sts, u.podFQDN(sts, 0)
}

func roleLabel(t universe.ClusterType) string {
	if t == universe.ReadReplica {
		return "read-replica"
	}
	return "primary"
}

func (u *Universe) labels(t universe.ClusterType, zone string) map[string]string {
	l := map[string]string{labelApp: appName, labelUniv: u.spec.Name, labelRole: roleLabel(t)}
	if zone != "" {
		l["yb-zone"] = zoneSlug(zone)
	}
	return kubemeta.TestLabels(l)
}

func (u *Universe) selector(t universe.ClusterType, zone string) map[string]string {
	l := map[string]string{labelApp: appName, labelUniv: u.spec.Name, labelRole: roleLabel(t)}
	if zone != "" {
		l["yb-zone"] = zoneSlug(zone)
	}
	return l
}

// gflagConf is the yugabyted config file for one cluster. yugabyted takes the
// flags as comma separated lists, so a value holding a comma cannot be carried.
func gflagConf(master, tserver map[string]string) (string, error) {
	for _, m := range []map[string]string{master, tserver} {
		for k, v := range m {
			if strings.ContainsAny(v, ",\"") {
				return "", fmt.Errorf("gflag %s=%q: yugabyted takes flags as a comma separated list, so a value cannot hold a comma or a quote", k, v)
			}
		}
	}
	csv := func(m map[string]string) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + m[k]
		}
		return strings.Join(parts, ",")
	}
	b, err := json.Marshal(map[string]string{"master_flags": csv(master), "tserver_flags": csv(tserver)})
	return string(b), err
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// clusterFlags are the gflags a cluster runs with: the universe's, then the
// cluster's own. YSQL listens on every address so a Service can reach it.
func (u *Universe) clusterFlags(c universe.ClusterSpec) (master, tserver map[string]string) {
	master = merge(u.spec.MasterGflags, c.MasterGflags)
	tserver = merge(map[string]string{"pgsql_proxy_bind_address": "0.0.0.0:5433", "ysql_enable_auth": "false"}, u.spec.TServerGflags, c.TServerGflags)
	return master, tserver
}

func confName(t universe.ClusterType) string { return "yugabyted-" + roleLabel(t) }

const startScript = `set -eu
cd /home/yugabyte
[ -e ybc ] || ln -s ybc-* ybc || echo "no ybc link: backups unavailable"
HOST="$(hostname -f)"
ARGS="--background=false --base_dir=/data --advertise_address=$HOST --cloud_location=$CLOUD_LOCATION --config=/conf/yugabyted.conf --ui=false --backup_daemon=true"
if [ "$JOIN_HOST" != "$HOST" ]; then
  until bin/ysqlsh -h "$JOIN_HOST" -p 5433 -U yugabyte -d yugabyte -c 'select 1' >/dev/null 2>&1; do sleep 3; done
  ARGS="$ARGS --join=$JOIN_HOST"
fi
if [ "$READ_REPLICA" = 1 ]; then ARGS="$ARGS --read_replica"; fi
exec bin/yugabyted start $ARGS
`

func (u *Universe) cloudLocation(zone string) string {
	if strings.Count(zone, ".") == 2 {
		return zone
	}
	region := u.regions[zone]
	if region == "" {
		region = "local"
	}
	return "k8s." + region + "." + universe.ZoneName(zone)
}

func (u *Universe) statefulSet(c universe.ClusterSpec, zone string, replicas int) map[string]any {
	name := stsName(c.Type, zone)
	_, join := u.firstPod()
	cpu := fmt.Sprint(c.Node.CPU)
	mem := fmt.Sprintf("%gGi", c.Node.MemoryGiB)
	podSpec := map[string]any{
		"terminationGracePeriodSeconds": 60,
		"securityContext":               map[string]any{"fsGroup": 1000},
		"imagePullSecrets":              []any{map[string]any{"name": pullSecret}},
		"containers": []any{map[string]any{
			"name": container, "image": u.image, "imagePullPolicy": "IfNotPresent",
			"command": []string{"bash", "-c", startScript},
			"env": []any{
				map[string]any{"name": "CLOUD_LOCATION", "value": u.cloudLocation(zone)},
				map[string]any{"name": "JOIN_HOST", "value": join},
				map[string]any{"name": "READ_REPLICA", "value": map[bool]string{true: "1", false: "0"}[c.Type == universe.ReadReplica]},
			},
			"ports": []any{
				map[string]any{"name": "ysql", "containerPort": ysqlPort},
				map[string]any{"name": "master-ui", "containerPort": 7000},
				map[string]any{"name": "tserver-ui", "containerPort": 9000},
			},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": cpu, "memory": mem},
				"limits":   map[string]any{"cpu": cpu, "memory": mem},
			},
			"volumeMounts": []any{
				map[string]any{"name": "data", "mountPath": dataMount},
				map[string]any{"name": "conf", "mountPath": confMount},
			},
			"readinessProbe": map[string]any{
				"exec": map[string]any{"command": []string{"bash", "-c",
					`/home/yugabyte/bin/ysqlsh -h "$(hostname -f)" -p 5433 -U yugabyte -d yugabyte -c "select 1" >/dev/null 2>&1`}},
				"initialDelaySeconds": 20, "periodSeconds": 10, "timeoutSeconds": 5, "failureThreshold": 90,
			},
		}},
		"volumes": []any{map[string]any{"name": "conf", "configMap": map[string]any{"name": confName(c.Type)}}},
	}
	if _, real := u.regions[zone]; real {
		podSpec["nodeSelector"] = map[string]any{"topology.kubernetes.io/zone": universe.ZoneName(zone)}
	}
	vct := map[string]any{
		"metadata": map[string]any{"name": "data"},
		"spec": map[string]any{
			"accessModes": []string{"ReadWriteOnce"},
			"resources":   map[string]any{"requests": map[string]any{"storage": fmt.Sprintf("%dGi", c.Node.DiskGiB)}},
		},
	}
	if c.Node.StorageClass != "" {
		vct["spec"].(map[string]any)["storageClassName"] = c.Node.StorageClass
	}
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]any{"name": name, "labels": u.labels(c.Type, zone)},
		"spec": map[string]any{
			"serviceName": u.spec.Name, "replicas": replicas, "podManagementPolicy": "Parallel",
			"selector": map[string]any{"matchLabels": u.selector(c.Type, zone)},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels":      u.labels(c.Type, zone),
					"annotations": map[string]any{"cluster-autoscaler.kubernetes.io/safe-to-evict": "false", "mekoqa/gflags": u.confHash(c)},
				},
				"spec": podSpec,
			},
			"volumeClaimTemplates": []any{vct},
		},
	}
}

func (u *Universe) confHash(c universe.ClusterSpec) string {
	m, t := u.clusterFlags(c)
	s, _ := gflagConf(m, t)
	sum := 0
	for _, b := range []byte(s) {
		sum = (sum*31 + int(b)) % 1000000007
	}
	return fmt.Sprint(sum)
}

func (u *Universe) configMap(c universe.ClusterSpec) (map[string]any, error) {
	m, t := u.clusterFlags(c)
	conf, err := gflagConf(m, t)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": confName(c.Type), "labels": u.labels(c.Type, "")},
		"data":     map[string]any{"yugabyted.conf": conf},
	}, nil
}

func (u *Universe) services() []map[string]any {
	ports := []any{
		map[string]any{"name": "ysql", "port": ysqlPort, "targetPort": ysqlPort},
		map[string]any{"name": "master-ui", "port": 7000, "targetPort": 7000},
		map[string]any{"name": "tserver-ui", "port": 9000, "targetPort": 9000},
	}
	svc := func(name string, headless bool, sel map[string]string) map[string]any {
		spec := map[string]any{"selector": sel, "ports": ports}
		if headless {
			spec["clusterIP"] = "None"
			spec["publishNotReadyAddresses"] = true
		}
		return map[string]any{"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"name": name, "labels": u.labels(universe.Primary, "")}, "spec": spec}
	}
	return []map[string]any{
		svc(u.spec.Name, true, map[string]string{labelApp: appName, labelUniv: u.spec.Name}),
		svc(u.ysqlService(universe.Primary), false, u.selector(universe.Primary, "")),
		svc(u.ysqlService(universe.ReadReplica), false, u.selector(universe.ReadReplica, "")),
	}
}

func (u *Universe) ysqlService(t universe.ClusterType) string {
	if t == universe.ReadReplica {
		return u.spec.Name + "-rr-ysql"
	}
	return u.spec.Name + "-ysql"
}

func render(docs ...map[string]any) (string, error) {
	var sb strings.Builder
	for i, d := range docs {
		out, err := yaml.Marshal(d)
		if err != nil {
			return "", err
		}
		if i > 0 {
			sb.WriteString("---\n")
		}
		sb.Write(out)
	}
	return sb.String(), nil
}
