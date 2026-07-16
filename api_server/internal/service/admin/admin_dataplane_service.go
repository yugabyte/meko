// Package admin: admin_dataplane_service surfaces live data-plane state for
// the meko_admin Data Plane tab by reading the deployment universe from
// meko_system.yb_universes and querying YugabyteDB Anywhere (YBA) for its
// configuration and utilization. Everything degrades gracefully: no YBA token
// -> Configured=false; token set but universe unreadable -> Available=false;
// so the endpoint never 500s on an infra hiccup.
package admin

import (
	"context"
	"log/slog"
	"time"

	adminrepo "meko/api_server/internal/repository/admin"
	"meko/api_server/internal/yba"
)

// YBA metric names requested for the utilization row (best-effort).
const (
	metricCPU    = "cpu_usage"
	metricMemory = "memory_usage"
	metricDisk   = "disk_usage"
)

// DataPlaneInfo is the GET /admin/dataplane payload. Configured is false when
// no YBA token is set; Available is false when the token is set but the
// universe couldn't be read (none registered, or YBA unreachable). The
// *_percent metrics are omitted when YBA doesn't return a parseable value.
type DataPlaneInfo struct {
	Configured        bool     `json:"configured"`
	Available         bool     `json:"available"`
	UniverseName      string   `json:"universe_name"`
	Provider          string   `json:"provider"`
	SoftwareVersion   string   `json:"software_version"`
	Nodes             int      `json:"nodes"`
	LiveNodes         int      `json:"live_nodes"`
	TotalNodes        int      `json:"total_nodes"`
	ReplicationFactor int      `json:"replication_factor"`
	InstanceType      string   `json:"instance_type"`
	TServerCores      float64  `json:"tserver_cores"`
	TServerMemoryGiB  float64  `json:"tserver_memory_gib"`
	MasterCores       float64  `json:"master_cores"`
	MasterMemoryGiB   float64  `json:"master_memory_gib"`
	Region            string   `json:"region"`
	VolumeSizeGB      int      `json:"volume_size_gb"`
	NumVolumes        int      `json:"num_volumes"`
	CPUPercent        *float64 `json:"cpu_percent,omitempty"`
	MemoryPercent     *float64 `json:"memory_percent,omitempty"`
	DiskPercent       *float64 `json:"disk_percent,omitempty"`
}

// AdminDataPlaneService reads the universe row and queries YBA. The YBA client
// is nil when no token is configured.
type AdminDataPlaneService struct {
	repo *adminrepo.AdminRepo
	yba  *yba.Client
}

// NewAdminDataPlaneService wires the admin repo and the (possibly nil) YBA client.
func NewAdminDataPlaneService(repo *adminrepo.AdminRepo, ybaClient *yba.Client) *AdminDataPlaneService {
	return &AdminDataPlaneService{repo: repo, yba: ybaClient}
}

// DataPlane returns the current data-plane snapshot, degrading gracefully.
func (s *AdminDataPlaneService) DataPlane(ctx context.Context) (DataPlaneInfo, error) {
	info := DataPlaneInfo{Configured: s.yba != nil}
	if s.yba == nil {
		return info, nil
	}

	row, err := s.repo.GetDefaultUniverse()
	if err != nil {
		return info, err
	}
	if row == nil {
		return info, nil // configured, but no universe registered yet
	}

	u, err := s.yba.GetUniverse(ctx, row.ManagementURL, row.UniverseUUID)
	if err != nil {
		slog.Error("admin dataplane: get universe", "universe", row.UniverseUUID, "error", err)
		return info, nil // token set but YBA read failed — degrade, don't 500
	}

	info.Available = true
	info.UniverseName = firstNonEmpty(u.Name, row.Name)
	info.Provider = u.Provider
	info.SoftwareVersion = u.SoftwareVersion
	info.Nodes = u.NumNodes
	info.LiveNodes = u.LiveNodes
	info.TotalNodes = u.TotalNodes
	info.ReplicationFactor = u.ReplicationFactor
	info.InstanceType = u.InstanceType
	info.TServerCores = u.TServerCores
	info.TServerMemoryGiB = u.TServerMemoryGiB
	info.MasterCores = u.MasterCores
	info.MasterMemoryGiB = u.MasterMemoryGiB
	info.Region = u.Region
	info.VolumeSizeGB = u.VolumeSizeGB
	info.NumVolumes = u.NumVolumes

	if m, mErr := s.yba.GetMetrics(ctx, row.ManagementURL, row.UniverseUUID, u.NodePrefix,
		[]string{metricCPU, metricMemory, metricDisk}, 10*time.Minute); mErr != nil {
		slog.Warn("admin dataplane: get metrics", "universe", row.UniverseUUID, "error", mErr)
	} else {
		info.CPUPercent = ptrIf(m, metricCPU)
		info.MemoryPercent = ptrIf(m, metricMemory)
		info.DiskPercent = ptrIf(m, metricDisk)
	}
	return info, nil
}

func ptrIf(m map[string]float64, key string) *float64 {
	if v, ok := m[key]; ok {
		return &v
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
