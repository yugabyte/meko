package functional

import (
	"fmt"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

func init() {
	core.Register(core.Test{
		Name: "functional/read_replica_basic", Suite: "functional", Category: core.Functional, Tags: tags("universe"), OwningTeam: core.TeamPlatform, Unguarded: true,
		Env: env(), Timeout: 40 * time.Minute,
		Description: "A universe gets a read replica cluster, a row written on the primary is read from the replica's node, and removing the replica leaves the primary serving.",
		Params:      []string{"db"},
		Run:         readReplicaBasic,
	})
}

func readReplicaBasic(t *core.T) error {
	set := stacktest.Universes(t)
	u, err := set.Create(t, universe.Single("rr", 1, 1))
	if err != nil {
		return finish(t, err)
	}
	rr := need[universe.ReadReplicas](t, u)

	if err := step(t, "add_read_replica", func(*core.StepHandle) error {
		return (rr.AddReadReplica(t, universe.ClusterSpec{RF: 1, Placement: map[string]int{universe.DefaultZone: 1}}))
	}); err != nil {
		return err
	}
	var replicaNode string
	if err := step(t, "replica_is_a_node", func(h *core.StepHandle) error {
		nodes, err := u.Nodes(t)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if n.Cluster == universe.ReadReplica {
				replicaNode = n.Name
			}
		}
		h.Set("replica", replicaNode)
		if replicaNode == "" {
			return fmt.Errorf("no read replica node among %d nodes", len(nodes))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := step(t, "write_on_primary", func(*core.StepHandle) error {
		_, err := u.SQL(t, "", "", "CREATE TABLE kv(k int primary key, v text); INSERT INTO kv VALUES (1, 'primary')")
		return err
	}); err != nil {
		return err
	}
	if err := step(t, "read_on_replica", func(*core.StepHandle) error {
		return waitRow(t, u, replicaNode, "", "SET yb_read_from_followers = true; SET default_transaction_read_only = true; SELECT v FROM kv WHERE k = 1", "primary", 3*time.Minute)
	}); err != nil {
		return err
	}
	return step(t, "remove_read_replica", func(*core.StepHandle) error {
		if err := rr.RemoveReadReplica(t); err != nil {
			return err
		}
		return waitRow(t, u, "", "", "SELECT count(*) FROM yb_servers()", "1", 5*time.Minute)
	})
}
