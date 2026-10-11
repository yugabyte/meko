package ybauniverse

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yba"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

var (
	_ universe.ReadReplicas      = (*Universe)(nil)
	_ universe.XCluster          = (*Universe)(nil)
	_ universe.XClusterBootstrap = (*Universe)(nil)
)

// AddReadReplica adds an ASYNC cluster shaped like the primary. Its zone is
// the provider's, as for a one-zone primary.
func (u *Universe) AddReadReplica(ctx context.Context, c universe.ClusterSpec) error {
	primary, err := u.cluster(ctx, "PRIMARY")
	if err != nil {
		return err
	}
	intent, ok := primary["userIntent"].(map[string]any)
	if !ok {
		return fmt.Errorf("universe %s: the primary cluster has no userIntent", u.spec.Name)
	}
	intent = maps.Clone(intent)
	// YBA refuses a read-only cluster that carries overrides.
	delete(intent, "universeOverrides")
	delete(intent, "azOverrides")
	delete(intent, "userIntentOverrides")
	intent["numNodes"] = c.Tservers()
	intent["replicationFactor"] = c.RF
	ref, err := u.p.Client.AddReadOnlyCluster(ctx, u.id, intent)
	if err != nil {
		return err
	}
	return u.settle(ctx, ref, "add a read replica")
}

func (u *Universe) RemoveReadReplica(ctx context.Context) error {
	rr, err := u.cluster(ctx, "ASYNC")
	if err != nil {
		return err
	}
	id, _ := rr["uuid"].(string)
	ref, err := u.p.Client.DeleteReadOnlyCluster(ctx, u.id, id)
	if err != nil {
		return err
	}
	return u.settle(ctx, ref, "remove the read replica")
}

func (u *Universe) cluster(ctx context.Context, clusterType string) (map[string]any, error) {
	raw, err := u.p.Client.GetUniverseJSON(ctx, u.id)
	if err != nil {
		return nil, err
	}
	details, _ := raw["universeDetails"].(map[string]any)
	clusters, _ := details["clusters"].([]any)
	for _, c := range clusters {
		if m, ok := c.(map[string]any); ok && m["clusterType"] == clusterType {
			return m, nil
		}
	}
	return nil, fmt.Errorf("universe %s has no %s cluster", u.spec.Name, clusterType)
}

func (u *Universe) settle(ctx context.Context, ref yba.TaskRef, what string) error {
	if _, err := u.p.Client.WaitTask(ctx, ref.TaskUUID); err != nil {
		return fmt.Errorf("%s on %s: %w", what, u.spec.Name, err)
	}
	return u.p.Client.WaitUniverseReady(ctx, u.id, createTimeout)
}

func (u *Universe) BootstrapXClusterVia(store universe.S3) {
	u.xcMu.Lock()
	u.xcStore = &store
	u.xcMu.Unlock()
}

// SetupXCluster creates a YBA DR config: database-scoped xCluster in automatic
// DDL mode, YBA's only way to that mode. YBA creates the databases on the
// target itself, by restoring a backup of the source.
func (u *Universe) SetupXCluster(ctx context.Context, target universe.Universe, databases []string) error {
	dst, ok := target.(*Universe)
	if !ok {
		return fmt.Errorf("xCluster from YBA universe %s needs a YBA target, got %s", u.spec.Name, target.Provider())
	}
	u.xcMu.Lock()
	store := u.xcStore
	u.xcMu.Unlock()
	if store == nil {
		return errors.New("YBA bootstraps xCluster through a backup: call BootstrapXClusterVia first")
	}
	cfg, err := u.storageConfig(ctx, *store)
	if err != nil {
		return err
	}
	nss, err := u.p.Client.ListNamespaces(ctx, u.id)
	if err != nil {
		return err
	}
	var ids []string
	for _, db := range databases {
		id := ""
		for _, ns := range nss {
			if ns.Name == db && ns.TableType == "PGSQL_TABLE_TYPE" {
				id = ns.UUID
			}
		}
		if id == "" {
			return fmt.Errorf("universe %s has no YSQL database %s", u.spec.Name, db)
		}
		ids = append(ids, id)
	}
	// YBA's connectivity precheck pings, and the DB image has no ping. The row
	// the test reads on the target proves the connectivity.
	if err := u.p.Client.SetRuntimeConfig(ctx, u.id, "yb.xcluster.network_connectivity_check.enabled", "false"); err != nil {
		return err
	}
	ref, err := u.p.Client.CreateDrConfig(ctx, yba.DrConfigSpec{
		Name: "mekoqa-" + short(u.id) + "-" + short(dst.id), SourceUUID: u.id, TargetUUID: dst.id,
		NamespaceUUIDs: ids, StorageConfigUUID: cfg,
	})
	if err != nil {
		return err
	}
	u.xcMu.Lock()
	if u.drs == nil {
		u.drs = map[string]string{}
	}
	u.drs[dst.id] = ref.ResourceUUID
	u.xcMu.Unlock()
	if _, err := u.p.Client.WaitTask(ctx, ref.TaskUUID); err != nil {
		return fmt.Errorf("set up xCluster %s -> %s: %w", u.spec.Name, dst.spec.Name, err)
	}
	return nil
}

func (u *Universe) XClusterStatus(ctx context.Context, target universe.Universe) (universe.XClusterStatus, error) {
	id, err := u.drTo(target)
	if err != nil {
		return universe.XClusterStatus{}, err
	}
	dr, err := u.p.Client.GetDrConfig(ctx, id)
	if err != nil {
		return universe.XClusterStatus{}, err
	}
	return universe.XClusterStatus{
		Healthy: dr.State == "Replicating" && dr.Status == "Running",
		Detail:  fmt.Sprintf("DR config %s: state %s, status %s, automatic DDL %t", dr.Name, dr.State, dr.Status, dr.AutomaticDdlMode),
	}, nil
}

func (u *Universe) TeardownXCluster(ctx context.Context, target universe.Universe) error {
	id, err := u.drTo(target)
	if err != nil {
		return err
	}
	ref, err := u.p.Client.DeleteDrConfig(ctx, id)
	if err != nil {
		return err
	}
	_, err = u.p.Client.WaitTask(ctx, ref.TaskUUID)
	return err
}

func (u *Universe) drTo(target universe.Universe) (string, error) {
	dst, ok := target.(*Universe)
	if !ok {
		return "", fmt.Errorf("no xCluster from %s to a %s universe", u.spec.Name, target.Provider())
	}
	u.xcMu.Lock()
	defer u.xcMu.Unlock()
	if id := u.drs[dst.id]; id != "" {
		return id, nil
	}
	return "", fmt.Errorf("no xCluster from %s to %s", u.spec.Name, dst.spec.Name)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
