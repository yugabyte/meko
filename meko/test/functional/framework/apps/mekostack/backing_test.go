package mekostack

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	execpkg "github.com/yugabyte/meko/meko/test/functional/framework/exec"
)

func ybBacking() *Backing {
	return &Backing{
		Mode: "yb_k8s", Describe: "YugabyteDB in mekoqa-yb-abc", Host: "yb-tservers.mekoqa-yb-abc.svc.cluster.local.", Port: 5433,
		Namespace: "mekoqa-yb-abc", Replication: 3, Tservers: 3,
	}
}

func TestValuesPointTheChartAtABacking(t *testing.T) {
	cfg := testConfig(t)
	cfg.Backing = ybBacking()
	dp, _ := NewDataPlaneSecrets(bytes.NewReader(bytes.Repeat([]byte{0xab}, 512)))
	v, err := cfg.Values(dp, MinIO{AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := MarshalValues(v)
	var back map[string]any
	if err := yaml.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	get := func(path ...string) any {
		var cur any = back
		for _, p := range path {
			m, _ := cur.(map[string]any)
			cur = m[p]
		}
		return cur
	}
	host := "yb-tservers.mekoqa-yb-abc.svc.cluster.local."
	for path, want := range map[string]any{
		"yugabyte.enabled":                    false,
		"apiServer.config.DB_HOST":            host,
		"apiServer.config.SYSTEM_DB_HOST":     host,
		"apiServer.config.LANGFUSE_DB_HOST":   host,
		"apiServer.config.DB_PORT":            "5433",
		"apiServer.config.DB_SSL_MODE":        "disable",
		"apiServer.staticArgs.langfuseDbPort": "5433",
		"apiServer.staticArgs.s3AuthMode":     "minio",
		"apiServer.config.S3_BUCKET":          "mekoqa",
		"mekoMcpServer.config.DB_HOST":        host,
		"mekoMcpServer.config.SYSTEM_DB_HOST": host,
		"dataPlaneSecrets.dbPassword":         dp.DBPassword,
		"ragAgent.dbConnectionString":         "postgresql://yugabyte:" + dp.DBPassword + "@" + host + ":5433/yugabyte",
		"apiServer.migrate.extensionsImage":   cfg.DatabaseImage,
	} {
		if got := get(strings.Split(path, ".")...); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	masked, _ := MarshalValues(MaskedValues(v))
	if strings.Contains(string(masked), dp.DBPassword) {
		t.Errorf("the masked values carry the database password:\n%s", masked)
	}
}

func TestValuesKeepTheBundledDatabaseWithoutABacking(t *testing.T) {
	for _, b := range []*Backing{nil, {Mode: ModeBundled}} {
		cfg := testConfig(t)
		cfg.Backing = b
		dp, _ := NewDataPlaneSecrets(nil)
		v, err := cfg.Values(dp, MinIO{})
		if err != nil {
			t.Fatal(err)
		}
		yb := v["yugabyte"].(map[string]any)
		if _, off := yb["enabled"]; off || yb["Image"] == nil {
			t.Errorf("backing %v: yugabyte = %v, want the bundled database's image and no enabled=false", b, yb)
		}
		if _, set := v["apiServer"].(map[string]any)["config"].(map[string]any)["DB_HOST"]; set {
			t.Errorf("backing %v: DB_HOST was set, the chart fills it for the bundled database", b)
		}
	}
}

func TestAnExternalPasswordGoesToTheChart(t *testing.T) {
	cfg := testConfig(t)
	cfg.Backing = &Backing{Mode: "external", Host: "db.example", Port: 5433, Password: "p@ss/w"}
	dp, _ := NewDataPlaneSecrets(nil)
	v, _ := cfg.Values(dp, MinIO{})
	if got := v["dataPlaneSecrets"].(map[string]any)["dbPassword"]; got != "p@ss/w" {
		t.Errorf("dbPassword = %v, want the external one", got)
	}
	if got := v["ragAgent"].(map[string]any)["dbConnectionString"]; got != "postgresql://yugabyte:p%40ss%2Fw@db.example:5433/yugabyte" {
		t.Errorf("dbConnectionString = %v, want the password escaped", got)
	}
}

func TestValidateRefusesABackingTheChartCannotUse(t *testing.T) {
	for name, b := range map[string]*Backing{
		"no host":                           {Mode: "external", Port: 5433},
		"no port":                           {Mode: "external", Host: "h"},
		"a user the mcp-server cannot take": {Mode: "external", Host: "h", Port: 5433, User: "meko"},
	} {
		cfg := testConfig(t)
		cfg.Backing = b
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate passed", name)
		}
	}
	cfg := testConfig(t)
	cfg.Backing = ybBacking()
	if err := cfg.Validate(); err != nil {
		t.Errorf("a good backing: %v", err)
	}
}

func backedCluster(extensions string) *cluster {
	base := deployCluster(quayManifest)
	return &cluster{handle: func(line string, c call) (execpkg.Result, bool) {
		switch {
		case strings.Contains(line, "exec mekoqa-ysql -c ysqlsh") && strings.Contains(line, "pg_available_extensions"):
			return out(extensions)
		case strings.Contains(line, "exec mekoqa-ysql -c ysqlsh"):
			return out("1\nUPDATE 1\n")
		}
		return base.handle(line, c)
	}}
}

func TestDeployWithABackingChecksItBeforeInstallingAndNeverTouchesATserverPod(t *testing.T) {
	cl := backedCluster("mage\npg_dist_rag\nvector\n")
	a := deployApp(t, cl)
	a.cfg.Backing = ybBacking()
	if err := a.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := cl.lines()
	order := []string{
		"kind: ConfigMap", "kind: Pod", "wait pod/mekoqa-ysql --for=condition=Ready",
		"pg_available_extensions", " upgrade --install ", "UPDATE meko_system.users",
	}
	prev := -1
	for _, want := range order {
		idx := -1
		for i := prev + 1; i < len(lines); i++ {
			if strings.Contains(lines[i], want) || stdinHas(cl, i, want) {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("%q did not follow the previous step; calls:\n%s", want, strings.Join(lines, "\n"))
		}
		prev = idx
	}
	if cl.count("yb-tserver") != 0 {
		t.Errorf("a stack on a backing still went looking for a bundled tserver pod: %v", lines)
	}
	var record, pod string
	for _, c := range cl.calls {
		switch {
		case strings.Contains(c.stdin, "kind: Pod"):
			pod = c.stdin
		case strings.Contains(c.stdin, "kind: ConfigMap"):
			record = c.stdin
		}
	}
	for _, want := range []string{"replication_factor: \"3\"", "tservers: \"3\"", "mode: yb_k8s", "kind: Secret"} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	for _, want := range []string{"image: " + a.cfg.DatabaseImage, "name: " + pullSecretName, "PGPASSWORD"} {
		if !strings.Contains(pod, want) {
			t.Errorf("the ysqlsh pod lacks %q:\n%s", want, pod)
		}
	}
	b, err := a.Backing(context.Background())
	if err != nil || b.Mode != "yb_k8s" || b.Tservers != 3 {
		t.Errorf("Backing = %+v, %v", b, err)
	}
}

func stdinHas(cl *cluster, i int, sub string) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return i < len(cl.calls) && strings.Contains(cl.calls[i].stdin, sub)
}

func TestDeployRefusesADatabaseThatIsNotTheMekoBuild(t *testing.T) {
	cl := backedCluster("plpgsql\nvector\n")
	a := deployApp(t, cl)
	a.cfg.Backing = ybBacking()
	err := a.Deploy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pg_dist_rag, mage") || !strings.Contains(err.Error(), "not the meko database build") {
		t.Fatalf("err = %v, want the missing extensions named", err)
	}
	if cl.count(" upgrade --install ") != 0 {
		t.Error("the chart was installed over a database that cannot host it")
	}
}

func TestABundledStackKeepsReachingItsTserverPod(t *testing.T) {
	cl := deployCluster(quayManifest)
	a := deployApp(t, cl)
	if err := a.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cl.count("exec yb-tserver-0 -c yb-tserver") == 0 || cl.count("mekoqa-ysql") != 0 {
		t.Errorf("a bundled stack must use its tserver pod and no ysqlsh pod: %v", cl.lines())
	}
	b, _ := a.Backing(context.Background())
	if b.Mode != ModeBundled || b.Tservers != 1 || b.Replication != 1 {
		t.Errorf("Backing = %+v, want the bundled single node", b)
	}
}

func TestAttachReadsTheBackingTheStackWasBuiltWith(t *testing.T) {
	record := `{"data":{"mode":"yb_k8s","host":"yb-tservers.ns.svc.cluster.local.","port":"5433","user":"yugabyte","namespace":"ns","replication_factor":"3","tservers":"3"}}`
	cl := backedCluster("")
	inner := cl.handle
	cl.handle = func(line string, c call) (execpkg.Result, bool) {
		if strings.Contains(line, "get namespace mekoqa-st-abc -o json") {
			return out(`{"metadata":{"labels":{}}}`)
		}
		if strings.Contains(line, "get service cognito-mock") {
			return notFound(`services "cognito-mock"`)
		}
		if strings.Contains(line, "get configmap mekoqa-backing -o json") {
			return out(record)
		}
		return inner(line, c)
	}
	a := deployApp(t, cl)
	if err := a.Attach(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := a.Backing(context.Background())
	if err != nil || b.Mode != "yb_k8s" || b.Replication != 3 || b.Namespace != "ns" {
		t.Fatalf("Backing = %+v, %v", b, err)
	}
	if cl.count("exec mekoqa-ysql") == 0 || cl.count("yb-tserver") != 0 {
		t.Errorf("an attached stack on a backing must reach it through the ysqlsh pod: %v", cl.lines())
	}
}

func TestAnUnrecordedStackIsTheBundledOne(t *testing.T) {
	cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		if strings.Contains(line, "get configmap mekoqa-backing") {
			return notFound(`configmaps "mekoqa-backing"`)
		}
		return execpkg.Result{}, false
	}}
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil)
	b, err := a.Backing(context.Background())
	if err != nil || b.Mode != ModeBundled {
		t.Errorf("Backing = %+v, %v, want bundled", b, err)
	}
}

func TestTheYsqlPodIsRecreatedWhenItIsGone(t *testing.T) {
	gone := true
	cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		if strings.Contains(line, "exec mekoqa-ysql") {
			if gone {
				gone = false
				return execpkg.Result{ExitCode: 1, Stderr: `Error from server (NotFound): pods "mekoqa-ysql" not found`}, true
			}
			return out("ok\n")
		}
		return execpkg.Result{}, false
	}}
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil)
	a.backing = ybBacking()
	got, err := a.ysql(context.Background(), "SELECT 1")
	if err != nil || got != "ok\n" {
		t.Fatalf("ysql = %q, %v", got, err)
	}
	created := 0
	for _, c := range cl.calls {
		if strings.Contains(c.stdin, "kind: Pod") {
			created++
		}
	}
	if created != 2 {
		t.Errorf("the ysqlsh pod was created %d times, want 2", created)
	}
}
