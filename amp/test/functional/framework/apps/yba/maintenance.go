// Planned maintenance on an existing universe: restart, gflag change, software
// upgrade. Each posts the universe's own cluster objects back verbatim apart
// from the changed field, since YBA reverts any field a hand-built payload omits.
package yba

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

type UpgradeOption string

const (
	// UpgradeRolling is the only option that keeps the universe serving.
	UpgradeRolling    UpgradeOption = "Rolling"
	UpgradeNonRolling UpgradeOption = "Non-Rolling"
	UpgradeNonRestart UpgradeOption = "Non-Restart"
)

// DefaultUpgradeSleep is generous: the node must be serving again before the next one restarts.
const DefaultUpgradeSleep = 180 * time.Second

// clusters stay raw so they round-trip except where we edit them.
type rawUniverse struct {
	UniverseDetails struct {
		NodePrefix string            `json:"nodePrefix"`
		Clusters   []json.RawMessage `json:"clusters"`
	} `json:"universeDetails"`
}

func (c *Client) upgradeBase(ctx context.Context, universeID string, sleep time.Duration) (map[string]any, []map[string]any, error) {
	var raw rawUniverse
	if _, err := c.doJSON(ctx, http.MethodGet, c.custURL("/universes/"+universeID), nil, &raw); err != nil {
		return nil, nil, fmt.Errorf("read universe %s: %w", universeID, err)
	}
	if len(raw.UniverseDetails.Clusters) == 0 {
		return nil, nil, fmt.Errorf("universe %s reports no clusters", universeID)
	}
	clusters := make([]map[string]any, 0, len(raw.UniverseDetails.Clusters))
	for i, rc := range raw.UniverseDetails.Clusters {
		var m map[string]any
		if err := json.Unmarshal(rc, &m); err != nil {
			return nil, nil, fmt.Errorf("decode cluster %d of universe %s: %w", i, universeID, err)
		}
		clusters = append(clusters, m)
	}
	if sleep <= 0 {
		sleep = DefaultUpgradeSleep
	}
	ms := sleep.Milliseconds()
	payload := map[string]any{
		"universeUUID":                   universeID,
		"nodePrefix":                     raw.UniverseDetails.NodePrefix,
		"sleepAfterMasterRestartMillis":  ms,
		"sleepAfterTServerRestartMillis": ms,
	}
	return payload, clusters, nil
}

func clusterSoftwareVersion(clusters []map[string]any) string {
	for _, cl := range clusters {
		ui, _ := cl["userIntent"].(map[string]any)
		if v, ok := ui["ybSoftwareVersion"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (c *Client) RestartUniverse(ctx context.Context, universeID string, opt UpgradeOption, sleep time.Duration) (TaskRef, error) {
	payload, clusters, err := c.upgradeBase(ctx, universeID, sleep)
	if err != nil {
		return TaskRef{}, err
	}
	payload["clusters"] = clusters
	payload["upgradeOption"] = string(opt)
	var out TaskRef
	_, err = c.doJSON(ctx, http.MethodPost, c.custURL("/universes/"+universeID+"/upgrade/restart"), payload, &out)
	return out, err
}

// YBA rejects an upgrade that changes nothing, so passing a value the universe already has is an error.
func (c *Client) UpgradeGflags(ctx context.Context, universeID string, master, tserver map[string]string, opt UpgradeOption, sleep time.Duration) (TaskRef, error) {
	if len(master) == 0 && len(tserver) == 0 {
		return TaskRef{}, fmt.Errorf("upgrade gflags: no gflags given")
	}
	payload, clusters, err := c.upgradeBase(ctx, universeID, sleep)
	if err != nil {
		return TaskRef{}, err
	}
	for _, cl := range clusters {
		ui, ok := cl["userIntent"].(map[string]any)
		if !ok {
			return TaskRef{}, fmt.Errorf("universe %s has a cluster with no userIntent", universeID)
		}
		ui["masterGFlags"] = mergeGflags(ui["masterGFlags"], master)
		ui["tserverGFlags"] = mergeGflags(ui["tserverGFlags"], tserver)
	}
	payload["clusters"] = clusters
	payload["ybSoftwareVersion"] = clusterSoftwareVersion(clusters)
	payload["taskType"] = "GFlags"
	payload["upgradeOption"] = string(opt)
	var out TaskRef
	_, err = c.doJSON(ctx, http.MethodPost, c.custURL("/universes/"+universeID+"/upgrade/gflags"), payload, &out)
	return out, err
}

func (c *Client) UpgradeSoftware(ctx context.Context, universeID, version string, opt UpgradeOption, sleep time.Duration) (TaskRef, error) {
	if version == "" {
		return TaskRef{}, fmt.Errorf("upgrade software: no target version")
	}
	payload, clusters, err := c.upgradeBase(ctx, universeID, sleep)
	if err != nil {
		return TaskRef{}, err
	}
	payload["clusters"] = clusters
	payload["ybSoftwareVersion"] = version
	payload["taskType"] = "Software"
	payload["upgradeOption"] = string(opt)
	payload["enableYbc"] = false
	var out TaskRef
	_, err = c.doJSON(ctx, http.MethodPost, c.custURL("/universes/"+universeID+"/upgrade/db_version"), payload, &out)
	return out, err
}

// Until finalized the universe stays rollback-capable and the new version's auto-flags are inert.
func (c *Client) FinalizeRequired(ctx context.Context, universeID, version string) (bool, error) {
	var out struct {
		FinalizeRequired bool `json:"finalizeRequired"`
	}
	_, err := c.doJSON(ctx, http.MethodPost,
		c.custURL("/universes/"+universeID+"/upgrade/software/precheck"),
		map[string]any{"ybSoftwareVersion": version}, &out)
	if err != nil {
		return false, fmt.Errorf("software upgrade precheck for %s: %w", version, err)
	}
	return out.FinalizeRequired, nil
}

func (c *Client) FinalizeUpgrade(ctx context.Context, universeID string) (TaskRef, error) {
	var out TaskRef
	_, err := c.doJSON(ctx, http.MethodPost,
		c.custURL("/universes/"+universeID+"/upgrade/finalize"), map[string]any{}, &out)
	return out, err
}

func (c *Client) RestartUniverseAndWait(ctx context.Context, universeID string, opt UpgradeOption, sleep time.Duration) error {
	ref, err := c.RestartUniverse(ctx, universeID, opt, sleep)
	if err != nil {
		return err
	}
	return c.waitUpgradeTask(ctx, universeID, "restart", ref)
}

func (c *Client) UpgradeGflagsAndWait(ctx context.Context, universeID string, master, tserver map[string]string, opt UpgradeOption, sleep time.Duration) error {
	ref, err := c.UpgradeGflags(ctx, universeID, master, tserver, opt, sleep)
	if err != nil {
		return err
	}
	return c.waitUpgradeTask(ctx, universeID, "gflag upgrade", ref)
}

func (c *Client) UpgradeSoftwareAndWait(ctx context.Context, universeID, version string, opt UpgradeOption, sleep time.Duration) error {
	ref, err := c.UpgradeSoftware(ctx, universeID, version, opt, sleep)
	if err != nil {
		return err
	}
	if err := c.waitUpgradeTask(ctx, universeID, "software upgrade", ref); err != nil {
		return err
	}
	required, err := c.FinalizeRequired(ctx, universeID, version)
	if err != nil {
		return err
	}
	if !required {
		return nil
	}
	c.log("yba: finalizing the upgrade to %s", version)
	fref, err := c.FinalizeUpgrade(ctx, universeID)
	if err != nil {
		return fmt.Errorf("finalize upgrade to %s: %w", version, err)
	}
	return c.waitUpgradeTask(ctx, universeID, "finalize upgrade", fref)
}

func (c *Client) waitUpgradeTask(ctx context.Context, universeID, what string, ref TaskRef) error {
	if ref.TaskUUID == "" {
		return fmt.Errorf("%s: YBA returned no task to wait on", what)
	}
	taskUUID := ref.TaskUUID
	for attempt := 1; ; attempt++ {
		task, err := c.WaitTask(ctx, taskUUID)
		if err == nil {
			return nil
		}
		if !KubectlFailed(task, err) {
			return fmt.Errorf("%s: %w", what, err)
		}
		if attempt > KubectlRetries {
			return core.AsEnvFailure(fmt.Errorf("%s: after %d retries: %w", what, KubectlRetries, err))
		}
		c.log("yba: %s task %s failed on a kubectl call; retrying it in %s (retry %d of %d)", what, taskUUID, KubectlRetryDelay, attempt, KubectlRetries)
		select {
		case <-ctx.Done():
			return core.AsEnvFailure(fmt.Errorf("%s: retry %d of %d cut short (%v): %w", what, attempt, KubectlRetries, ctx.Err(), err))
		case <-time.After(KubectlRetryDelay):
		}
		r, rerr := c.RetryTask(ctx, taskUUID)
		if rerr != nil {
			id, lerr := c.retriedTask(ctx, universeID, taskUUID)
			if lerr != nil {
				return core.AsEnvFailure(fmt.Errorf("%s: retrying task %s after %q: %w (%v)", what, taskUUID, err.Error(), rerr, lerr))
			}
			c.log("yba: retrying %s task %s errored (%v); waiting on the universe's task %s", what, taskUUID, rerr, id)
			r.TaskUUID = id
		}
		taskUUID = r.TaskUUID
	}
}

// mergeGflags accepts either shape YBA stores gflags in and writes the list form.
func mergeGflags(existing any, overrides map[string]string) []map[string]string {
	kept := map[string]string{}
	switch e := existing.(type) {
	case map[string]any:
		for k, v := range e {
			kept[k] = fmt.Sprint(v)
		}
	case []any:
		for _, item := range e {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			if name == "" {
				continue
			}
			kept[name] = fmt.Sprint(m["value"])
		}
	}
	for k, v := range overrides {
		kept[k] = v
	}
	out := make([]map[string]string, 0, len(kept))
	for _, name := range sortedGflagNames(kept) {
		out = append(out, map[string]string{"name": name, "value": kept[name]})
	}
	return out
}

func sortedGflagNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (c *Client) RefreshReleases(ctx context.Context) error {
	var out any
	_, err := c.doJSON(ctx, http.MethodPut, c.custURL("/releases"), nil, &out)
	return err
}

// Imports the helm chart for a kubernetes universe (YBA bundles one only for its own version), or the el8 tarball for a VM one.
func (c *Client) EnsureReleaseAvailable(ctx context.Context, version, arch string, k8s bool) error {
	if version == "" {
		return fmt.Errorf("ensure release available: empty version")
	}
	have := func(r Release) bool { return hasArch(r, ybaArch(arch)) }
	if k8s {
		have = hasK8sArtifact
	}
	present, err := c.releaseArtifactPresent(ctx, version, have)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if err := c.RefreshReleases(ctx); err != nil {
		c.log("yba: release refresh failed (continuing to an explicit import): %v", err)
	} else if present, err := c.releaseArtifactPresent(ctx, version, have); err == nil && present {
		return nil
	}

	if k8s {
		if err := c.EnsureReleaseK8s(ctx, version); err != nil {
			return fmt.Errorf("the %s helm chart is not available on this YBA and could not be imported: %w", version, err)
		}
		return nil
	}
	url := ReleaseTarballURL(version, arch)
	c.log("yba: %s is not in the release catalog; importing it from %s", version, url)
	if err := c.EnsureRelease(ctx, version, arch, url); err != nil {
		return fmt.Errorf("release %s is not available on this YBA and could not be imported: %w", version, err)
	}
	return nil
}

func (c *Client) releaseArtifactPresent(ctx context.Context, version string, have func(Release) bool) (bool, error) {
	releases, err := c.GetReleases(ctx)
	if err != nil {
		return false, err
	}
	r, ok := findRelease(releases, version)
	return ok && have(r), nil
}

// PreviousReleaseTrain: "2026.1.0.0-b1" -> "2025.2". Two trains a year, so the one before YYYY.1 is the previous year's .2.
func PreviousReleaseTrain(version string) string {
	v, err := core.ParseVersion(version)
	if err != nil {
		return ""
	}
	parts := strings.SplitN(v.Base(), ".", 3)
	if len(parts) < 2 {
		return ""
	}
	year, err1 := strconv.Atoi(parts[0])
	train, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || year <= 0 {
		return ""
	}
	if train > 1 {
		return fmt.Sprintf("%d.%d", year, train-1)
	}
	return fmt.Sprintf("%d.2", year-1)
}

func (c *Client) EditUniverseNodes(ctx context.Context, universeID string, numNodes int) (TaskRef, error) {
	if numNodes <= 0 {
		return TaskRef{}, fmt.Errorf("edit universe %s: num nodes must be positive, got %d", universeID, numNodes)
	}
	ref, err := c.editUniverse(ctx, universeID, func(params map[string]any) error {
		return setPrimaryNumNodes(params, numNodes)
	})
	if err != nil {
		return TaskRef{}, fmt.Errorf("edit universe %s to %d nodes: %w", universeID, numNodes, err)
	}
	c.log("yba: editing universe %s to %d nodes (task %s)", universeID, numNodes, ref.TaskUUID)
	return ref, nil
}

// GrowUniverseVolumes raises the primary cluster's tserver volume size by
// addGB. Changing nothing but the size is what makes YBA expand a Kubernetes
// universe's PVCs in place instead of planning a full move.
func (c *Client) GrowUniverseVolumes(ctx context.Context, universeID string, addGB int) (from, to int, ref TaskRef, err error) {
	if addGB <= 0 {
		return 0, 0, TaskRef{}, fmt.Errorf("grow universe %s volumes: the increase must be positive, got %d GB", universeID, addGB)
	}
	ref, err = c.editUniverse(ctx, universeID, func(params map[string]any) error {
		from, to, err = growPrimaryVolumeSize(params, addGB)
		return err
	})
	if err != nil {
		return 0, 0, TaskRef{}, fmt.Errorf("grow universe %s volumes by %d GB: %w", universeID, addGB, err)
	}
	c.log("yba: growing universe %s tserver volumes %d -> %d GB (task %s)", universeID, from, to, ref.TaskUUID)
	return from, to, ref, nil
}

func (c *Client) GrowUniverseVolumesAndWait(ctx context.Context, universeID string, addGB int, timeout time.Duration) (from, to int, err error) {
	from, to, ref, err := c.GrowUniverseVolumes(ctx, universeID, addGB)
	if err != nil {
		return 0, 0, err
	}
	if err := c.waitUpgradeTask(ctx, universeID, fmt.Sprintf("grow volumes to %d GB", to), ref); err != nil {
		return from, to, err
	}
	return from, to, c.WaitUniverseReady(ctx, universeID, timeout)
}

// The configure-then-edit flow is required: a hand-built payload that skips
// /universe_configure leaves placementInfo stale, and YBA then either rejects
// the edit or moves the wrong node.
func (c *Client) editUniverse(ctx context.Context, universeID string, mutate func(params map[string]any) error) (TaskRef, error) {
	var raw struct {
		UniverseDetails map[string]any `json:"universeDetails"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, c.custURL("/universes/"+universeID), nil, &raw); err != nil {
		return TaskRef{}, fmt.Errorf("read universe: %w", err)
	}
	params := raw.UniverseDetails
	if params == nil {
		return TaskRef{}, fmt.Errorf("the universe reports no universeDetails")
	}
	if err := mutate(params); err != nil {
		return TaskRef{}, err
	}
	params["universeUUID"] = universeID
	params["clusterOperation"] = "EDIT"
	params["currentClusterType"] = "PRIMARY"
	// -1 skips YBA's optimistic-concurrency check, which a universe being
	// written to by a live workload can lose to its own background updates.
	params["expectedUniverseVersion"] = -1

	var configured map[string]any
	if _, err := c.doJSON(ctx, http.MethodPost, c.custURL("/universe_configure"), params, &configured); err != nil {
		return TaskRef{}, fmt.Errorf("configure: %w", err)
	}
	var out TaskRef
	if _, err := c.doJSON(ctx, http.MethodPut, c.custURL("/universes/"+universeID), configured, &out); err != nil {
		return TaskRef{}, fmt.Errorf("submit: %w", err)
	}
	return out, nil
}

// Waits for the universe to be ready, not just for the task to end: an add is
// done once the new node serves, a remove once the drained one is gone.
func (c *Client) EditUniverseNodesAndWait(ctx context.Context, universeID string, numNodes int, timeout time.Duration) error {
	ref, err := c.EditUniverseNodes(ctx, universeID, numNodes)
	if err != nil {
		return err
	}
	if err := c.waitUpgradeTask(ctx, universeID, fmt.Sprintf("edit to %d nodes", numNodes), ref); err != nil {
		return err
	}
	return c.WaitUniverseReady(ctx, universeID, timeout)
}

// The primary cluster is the one carrying the tservers amp writes through. An
// async read replica has its own count and must not be touched.
func setPrimaryNumNodes(params map[string]any, numNodes int) error {
	ui, err := primaryUserIntent(params)
	if err != nil {
		return err
	}
	ui["numNodes"] = numNodes
	return nil
}

func primaryUserIntent(params map[string]any) (map[string]any, error) {
	clusters, ok := params["clusters"].([]any)
	if !ok || len(clusters) == 0 {
		return nil, fmt.Errorf("no clusters in the universe details")
	}
	for _, raw := range clusters {
		cl, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cl["clusterType"].(string); t != "" && !strings.EqualFold(t, "PRIMARY") {
			continue
		}
		ui, ok := cl["userIntent"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the primary cluster has no userIntent")
		}
		return ui, nil
	}
	return nil, fmt.Errorf("no primary cluster among the %d clusters", len(clusters))
}

func growPrimaryVolumeSize(params map[string]any, addGB int) (from, to int, err error) {
	ui, err := primaryUserIntent(params)
	if err != nil {
		return 0, 0, err
	}
	dev, ok := ui["deviceInfo"].(map[string]any)
	if !ok {
		return 0, 0, fmt.Errorf("the primary cluster's userIntent has no deviceInfo")
	}
	size, ok := dev["volumeSize"].(float64)
	if !ok || size <= 0 {
		return 0, 0, fmt.Errorf("the primary cluster's deviceInfo has no volumeSize (%v)", dev["volumeSize"])
	}
	from, to = int(size), int(size)+addGB
	dev["volumeSize"] = to
	return from, to, nil
}

var (
	retryLockTimeout = 2 * time.Minute
	retryLockPoll    = 3 * time.Second
)

// retriedTask finds the task a retry POST started when the POST errored, since
// YBA may have accepted it anyway.
func (c *Client) retriedTask(ctx context.Context, universeID, failed string) (string, error) {
	deadline := time.Now().Add(retryLockTimeout)
	last := "never read"
	for {
		u, err := c.GetUniverse(ctx, universeID)
		if err != nil {
			last = err.Error()
		} else {
			d := u.UniverseDetails
			last = fmt.Sprintf("updateInProgress=%v updatingTaskUUID=%q", d.UpdateInProgress, d.UpdatingTaskUUID)
			if d.UpdateInProgress && d.UpdatingTaskUUID != "" && d.UpdatingTaskUUID != failed {
				return d.UpdatingTaskUUID, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no retried task locked universe %s within %s (last read: %s)", universeID, retryLockTimeout, last)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(retryLockPoll):
		}
	}
}
