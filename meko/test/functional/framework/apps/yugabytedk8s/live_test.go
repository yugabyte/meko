//go:build live

package yugabytedk8s

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

// go test -tags live -run TestLive -v ./framework/apps/yugabytedk8s
// with LIVE_KUBE_CONTEXT and LIVE_QUAY=user:token set.
func liveProvider(t *testing.T) *Provider {
	kc, q := os.Getenv("LIVE_KUBE_CONTEXT"), os.Getenv("LIVE_QUAY")
	if kc == "" || q == "" {
		t.Skip("LIVE_KUBE_CONTEXT and LIVE_QUAY are not set")
	}
	user, tok, _ := strings.Cut(q, ":")
	return New(Config{KubeContext: kc, RunID: "live", PullUser: user, PullToken: tok, TTL: 2 * time.Hour, Logf: t.Logf})
}

func TestLiveRF3(t *testing.T) {
	ctx := context.Background()
	p := liveProvider(t)
	u, err := p.Create(ctx, universe.Single("l3", 3, 3))
	defer func() {
		if os.Getenv("LIVE_KEEP") == "" {
			_ = u.Teardown(context.Background())
		}
	}()
	if err != nil {
		t.Fatal(err)
	}
	out, err := u.SQL(ctx, "", "", "SELECT count(*) FROM yb_servers()")
	t.Logf("servers %q %v", out, err)
	out, err = u.SQL(ctx, "", "", "SELECT COALESCE(max(array_length(replicas, 1)), 0) FROM yb_tablet_metadata")
	t.Logf("rf %q %v", out, err)
	out, err = u.SQL(ctx, "", "", "SELECT name FROM pg_available_extensions WHERE name IN ('vector','pg_dist_rag','mage')")
	t.Logf("ext %q %v", out, err)
}

func TestLiveXClusterAndReadReplica(t *testing.T) {
	ctx := context.Background()
	p := liveProvider(t)
	src, err := p.Create(ctx, universe.Single("xs", 1, 1))
	if src != nil {
		defer src.Teardown(context.Background())
	}
	if err != nil {
		t.Fatal(err)
	}
	dst, err := p.Create(ctx, universe.Single("xt", 1, 1))
	if dst != nil {
		defer dst.Teardown(context.Background())
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.SQL(ctx, "", "", "CREATE DATABASE app"); err != nil {
		t.Fatal(err)
	}
	if err := src.SetupXCluster(ctx, dst, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.SQL(ctx, "", "app", "CREATE TABLE t(a int primary key, b text)"); err != nil {
		t.Fatal(err)
	}
	st, err := src.XClusterStatus(ctx, dst)
	t.Logf("status %+v %v", st, err)
	if _, err := src.SQL(ctx, "", "app", "INSERT INTO t VALUES (1,'x')"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		out, err := dst.SQL(ctx, "", "app", "SELECT count(*) FROM t")
		t.Logf("target %q %v", out, err)
		if strings.TrimSpace(out) == "1" {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if err := src.AddReadReplica(ctx, universe.ClusterSpec{RF: 1, Placement: map[string]int{universe.DefaultZone: 1}}); err != nil {
		t.Fatal(err)
	}
	out, err := src.SQL(ctx, "", "", "SELECT count(*) FROM yb_servers()")
	t.Logf("servers with RR %q %v", out, err)
}
