package universe

import "fmt"

// Getter is the part of the run configuration FromConfig reads.
type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetFloat(key string, def float64) float64
	GetMap(key string, def map[string]any) map[string]any
	GetStringSlice(key string, def []string) []string
}

// FromConfig reads the spec under prefix (db for the stack's database):
//
//	provider, image, version, rf, nodes, zones (list), cpu, memory_gib, disk_gib,
//	storage_class, instance_type, master_gflags, tserver_gflags, ysql_auth, tls
//
// nodes is the tserver count (0 means rf) and is spread evenly over zones. A
// spec read from the configuration is a single primary cluster.
func FromConfig(get Getter, prefix, name string) Spec {
	k := func(s string) string { return prefix + "." + s }
	rf := get.GetInt(k("rf"), DefaultRF)
	nodes := get.GetInt(k("nodes"), 0)
	if nodes <= 0 {
		nodes = rf
	}
	zones := get.GetStringSlice(k("zones"), nil)
	if len(zones) == 0 {
		zones = []string{DefaultZone}
	}
	placement := map[string]int{}
	for i := 0; i < nodes; i++ {
		placement[zones[i%len(zones)]]++
	}
	flags := func(key string) map[string]string {
		out := map[string]string{}
		for fk, v := range get.GetMap(k(key), nil) {
			out[fk] = toString(v)
		}
		return out
	}
	return Spec{
		Name:     name,
		Provider: Provider(get.GetString(k("provider"), string(Yugabyted))),
		Image:    get.GetString(k("image"), ""),
		Version:  get.GetString(k("version"), ""),
		TLS:      TLSSpec{Nodes: get.GetString(k("tls"), "") == "true"},
		Clusters: []ClusterSpec{{
			Type: Primary, RF: rf, Placement: placement,
			Node: NodeSpec{
				CPU: get.GetFloat(k("cpu"), DefaultCPU), MemoryGiB: get.GetFloat(k("memory_gib"), DefaultMemory),
				DiskGiB: get.GetInt(k("disk_gib"), DefaultDiskGiB), StorageClass: get.GetString(k("storage_class"), ""),
				InstanceType: get.GetString(k("instance_type"), ""),
			},
		}},
		MasterGflags:  flags("master_gflags"),
		TServerGflags: flags("tserver_gflags"),
	}
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}
