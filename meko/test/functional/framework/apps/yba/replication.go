package yba

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// AddReadOnlyCluster adds an ASYNC cluster with userIntent to a universe. YBA
// configures it from the universe's primary, which it finds by universeUUID.
func (c *Client) AddReadOnlyCluster(ctx context.Context, universeUUID string, userIntent map[string]any) (TaskRef, error) {
	body := map[string]any{
		"universeUUID": universeUUID,
		"clusters":     []any{map[string]any{"clusterType": "ASYNC", "userIntent": userIntent}},
	}
	var ref TaskRef
	if _, err := c.doJSON(ctx, http.MethodPost, c.custURL("/universes/"+universeUUID+"/clusters/read_only"), body, &ref); err != nil {
		return TaskRef{}, fmt.Errorf("add a read replica to %s: %w", universeUUID, err)
	}
	return ref, nil
}

func (c *Client) DeleteReadOnlyCluster(ctx context.Context, universeUUID, clusterUUID string) (TaskRef, error) {
	var ref TaskRef
	u := c.custURL("/universes/" + universeUUID + "/clusters/read_only/" + clusterUUID + "?isForceDelete=true")
	if _, err := c.doJSON(ctx, http.MethodDelete, u, nil, &ref); err != nil {
		return TaskRef{}, fmt.Errorf("delete read replica %s of %s: %w", clusterUUID, universeUUID, err)
	}
	return ref, nil
}

// GetUniverseJSON is the universe as YBA returns it, for the fields this
// package does not model, such as a cluster's whole userIntent.
func (c *Client) GetUniverseJSON(ctx context.Context, universeUUID string) (map[string]any, error) {
	var out map[string]any
	if _, err := c.doJSON(ctx, http.MethodGet, c.custURL("/universes/"+universeUUID), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type Namespace struct {
	UUID      string `json:"namespaceUUID"`
	Name      string `json:"name"`
	TableType string `json:"tableType"`
}

func (c *Client) ListNamespaces(ctx context.Context, universeUUID string) ([]Namespace, error) {
	var out []Namespace
	if _, err := c.doJSON(ctx, http.MethodGet, c.custURL("/universes/"+universeUUID+"/namespaces"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DrConfigSpec is a database-scoped xCluster DR config. YBA copies the
// databases to the target through a backup in StorageConfigUUID.
type DrConfigSpec struct {
	Name              string
	SourceUUID        string
	TargetUUID        string
	NamespaceUUIDs    []string
	StorageConfigUUID string
}

type DrConfig struct {
	UUID             string `json:"uuid"`
	Name             string `json:"name"`
	State            string `json:"state"`
	Status           string `json:"status"`
	AutomaticDdlMode bool   `json:"automaticDdlMode"`
}

func (c *Client) CreateDrConfig(ctx context.Context, s DrConfigSpec) (TaskRef, error) {
	body := map[string]any{
		"name":               s.Name,
		"sourceUniverseUUID": s.SourceUUID,
		"targetUniverseUUID": s.TargetUUID,
		"dbs":                s.NamespaceUUIDs,
		"bootstrapParams": map[string]any{
			"backupRequestParams": map[string]any{"storageConfigUUID": s.StorageConfigUUID},
		},
	}
	var ref TaskRef
	if _, err := c.doJSON(ctx, http.MethodPost, c.custURL("/dr_configs"), body, &ref); err != nil {
		return TaskRef{}, fmt.Errorf("create DR config %s: %w", s.Name, err)
	}
	return ref, nil
}

func (c *Client) GetDrConfig(ctx context.Context, uuid string) (DrConfig, error) {
	var out DrConfig
	if _, err := c.doJSON(ctx, http.MethodGet, c.custURL("/dr_configs/"+uuid), nil, &out); err != nil {
		return DrConfig{}, err
	}
	return out, nil
}

func (c *Client) DeleteDrConfig(ctx context.Context, uuid string) (TaskRef, error) {
	var ref TaskRef
	q := url.Values{"isForceDelete": {"true"}}
	if _, err := c.doJSON(ctx, http.MethodDelete, c.custURL("/dr_configs/"+uuid+"?"+q.Encode()), nil, &ref); err != nil {
		return TaskRef{}, fmt.Errorf("delete DR config %s: %w", uuid, err)
	}
	return ref, nil
}
