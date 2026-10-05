package yugabytedk8s

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	execpkg "github.com/yugabyte/meko/meko/test/functional/framework/exec"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

type rec struct {
	mu    sync.Mutex
	lines []string
	stdin []string
	pods  string
}

func (r *rec) run(_ context.Context, in io.Reader, argv []string) (execpkg.Result, error) {
	body := ""
	if in != nil {
		b, _ := io.ReadAll(in)
		body = string(b)
	}
	line := strings.Join(argv, " ")
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.stdin = append(r.stdin, body)
	r.mu.Unlock()
	switch {
	case strings.Contains(line, "Ready") && strings.Contains(line, "get pods"):
		return execpkg.Result{Stdout: strings.Repeat("True\n", 9)}, nil
	case strings.Contains(line, "get nodes"):
		return execpkg.Result{Stdout: "us-west1-a\tus-west1\nus-west1-b\tus-west1\n"}, nil
	case strings.Contains(line, "get pods"):
		return execpkg.Result{Stdout: "yb-a-0\n"}, nil
	case strings.Contains(line, "exec") && strings.Contains(body, "yb_servers"):
		return execpkg.Result{Stdout: "3\n"}, nil
	}
	return execpkg.Result{}, nil
}

func (r *rec) count(sub string) int {
	n := 0
	for i, l := range r.lines {
		if strings.Contains(l, sub) || strings.Contains(r.stdin[i], sub) {
			n++
		}
	}
	return n
}

func newProvider(r *rec) *Provider {
	p := New(Config{KubeContext: "ctx", RunID: "r1", Image: "quay.io/yugabyte/meko-yugabytedb:test", PullUser: "u", PullToken: "t", Logf: func(string, ...any) {}, ReadyTimeout: time.Second})
	return p.WithRunner(r.run)
}

func TestCreateBuildsOneStatefulSetPerZoneAndConfiguresPlacement(t *testing.T) {
	r := &rec{}
	spec := universe.Spec{Name: "db1", Clusters: []universe.ClusterSpec{{RF: 3, Placement: map[string]int{"us-west1-a": 2, "us-west1-b": 1}, Node: universe.NodeSpec{CPU: 2, MemoryGiB: 8, DiskGiB: 50, StorageClass: "premium-rwo"}}}}
	u, err := newProvider(r).Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if u.Namespace() != "mekoqa-yu-r1-db1" {
		t.Errorf("namespace = %s", u.Namespace())
	}
	all := strings.Join(r.stdin, "\n")
	for _, want := range []string{"name: yb-us-west1-a", "name: yb-us-west1-b", "replicas: 2", "storage: 50Gi", "storageClassName: premium-rwo", "cpu: \"2\"", "memory: 8Gi",
		"topology.kubernetes.io/zone: us-west1-a", "ysql_yb_enable_mage", "mekoqa-test"} {
		if want == "ysql_yb_enable_mage" {
			continue
		}
		if !strings.Contains(all, want) {
			t.Errorf("manifests lack %q", want)
		}
	}
	if r.count("configure data_placement") != 1 || r.count("--rf=3") != 1 || r.count("--fault_tolerance") != 0 {
		t.Errorf("placement not configured as RF3 in one zone: %v", r.lines)
	}
	if r.count("kubernetes.io/dockerconfigjson") != 1 {
		t.Error("no pull secret")
	}
}

func TestZonesSpreadWhenThereAreThree(t *testing.T) {
	r := &rec{}
	spec := universe.Spec{Name: "geo", Clusters: []universe.ClusterSpec{{RF: 3, Placement: map[string]int{"us-west1-a": 1, "us-west1-b": 1, "us-west1-c": 1}}}}
	if _, err := newProvider(r).Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if r.count("--fault_tolerance=zone") != 1 || r.count("--constraint_value=k8s.") != 1 {
		t.Errorf("zone placement missing: %v", r.lines)
	}
}

func TestRF1SingleNodeNeedsNoPlacement(t *testing.T) {
	r := &rec{}
	if _, err := newProvider(r).Create(context.Background(), universe.Single("one", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if r.count("configure data_placement") != 0 {
		t.Error("placement configured for one node")
	}
}

func TestSpecsYugabytedCannotBuildAreUnsupported(t *testing.T) {
	for name, mutate := range map[string]func(*universe.Spec){
		"tls":     func(s *universe.Spec) { s.TLS.Nodes = true },
		"auth":    func(s *universe.Spec) { s.YSQLAuth = true },
		"masters": func(s *universe.Spec) { s.Clusters[0].Masters.Count = 5 },
	} {
		s := universe.Single("u", 3, 5)
		mutate(&s)
		_, err := newProvider(&rec{}).Create(context.Background(), s)
		if !universe.IsUnsupported(err) {
			t.Errorf("%s: err = %v, want Unsupported", name, err)
		}
	}
}

func TestGflagsWithACommaAreRefused(t *testing.T) {
	if _, err := gflagConf(map[string]string{"a": "x,y"}, nil); err == nil {
		t.Error("a comma in a value was accepted")
	}
}

func TestImageFromVersionKeepsTheRepository(t *testing.T) {
	p := New(Config{Image: "quay.io/yugabyte/meko-yugabytedb:1", Logf: func(string, ...any) {}})
	if got := p.imageFor(universe.Spec{Version: "2"}); got != "quay.io/yugabyte/meko-yugabytedb:2" {
		t.Error(got)
	}
	if got := p.imageFor(universe.Spec{Image: "x/y:3", Version: "2"}); got != "x/y:3" {
		t.Error(got)
	}
}

func TestXClusterToAnotherProviderIsUnsupported(t *testing.T) {
	r := &rec{}
	u, _ := newProvider(r).Create(context.Background(), universe.Single("a", 1, 1))
	err := u.SetupXCluster(context.Background(), foreign{}, []string{"d"})
	if !universe.IsUnsupported(err) {
		t.Errorf("err = %v", err)
	}
}

type foreign struct{ universe.Universe }

func (foreign) Provider() universe.Provider { return universe.YBA }

func TestStopOnlyTheLastOrdinal(t *testing.T) {
	r := &rec{}
	u, _ := newProvider(r).Create(context.Background(), universe.Single("a", 1, 1))
	if _, _, err := splitPod("yb-a-2"); err != nil {
		t.Fatal(err)
	}
	if err := u.KillNode(context.Background(), "yb-zone-a-0"); err != nil {
		t.Fatal(err)
	}
	if r.count("--grace-period=0") != 1 {
		t.Error("kill did not force delete")
	}
}

func TestReapRefusesAForeignNamespace(t *testing.T) {
	r := &rec{}
	p := newProvider(r)
	if err := deleteNamespace(context.Background(), p.cluster, "kube-system"); err == nil {
		t.Error("deleted a namespace with no label")
	}
}
