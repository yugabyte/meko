package sched

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

// Against a real cluster: MEKOQA_SLOTS_KUBECONTEXT=<ctx> go test -run KubeSlotStore ./framework/sched/
// It holds and releases real slots in default/ampqa-cluster-slots, so never
// point it at a cluster whose slots are fully held by a run you care about.
func TestKubeSlotStoreAgainstACluster(t *testing.T) {
	kctx := os.Getenv("MEKOQA_SLOTS_KUBECONTEXT")
	if kctx == "" {
		t.Skip("MEKOQA_SLOTS_KUBECONTEXT not set")
	}
	st := newKubeSlotStore(k8s.New(k8s.Config{Context: kctx}))
	c := newClusterSlots(st, DefaultClusterSlots, t.Logf)
	c.poll = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	relA, err := c.Acquire(ctx, "slots-selftest/a", 1)
	if err != nil {
		t.Fatal(err)
	}
	relB, err := c.Acquire(ctx, "slots-selftest/b", 1)
	if err != nil {
		t.Fatal(err)
	}
	m, v, err := st.load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mine := 0
	for _, e := range m {
		if e.Held && (e.Holder == "slots-selftest/a" || e.Holder == "slots-selftest/b") {
			mine++
		}
	}
	if mine != 2 {
		t.Errorf("want both self-test holders in the ConfigMap, got %d: %+v", mine, m)
	}
	if conflict, err := st.store(ctx, m, v+"0"); err != nil || !conflict {
		t.Errorf("a stale resourceVersion must be a conflict, got conflict=%v err=%v", conflict, err)
	}
	relA()
	relB()
	m, _, _ = st.load(ctx)
	for _, e := range m {
		if e.Holder == "slots-selftest/a" || e.Holder == "slots-selftest/b" {
			t.Errorf("released entry still present: %+v", e)
		}
	}
}
