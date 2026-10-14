package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// GetRaw is GET {apiURL}{path}?{q} decoded into out, for contract probes the
// typed methods deliberately cannot express (a zero limit, a negative offset, a
// response read as raw JSON to search it). It returns the HTTP status alongside
// the error so a caller can assert on the code of a 2xx too.
func (c *Client) GetRaw(ctx context.Context, path string, q url.Values, out any) (int, error) {
	u := c.apiURL + path
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	status, _, err := c.doJSON(ctx, http.MethodGet, u, nil, out)
	return status, err
}

// PostRaw is POST {apiURL}{path} with body, decoded into out.
func (c *Client) PostRaw(ctx context.Context, path string, body, out any) (int, http.Header, error) {
	return c.doJSON(ctx, http.MethodPost, c.apiURL+path, body, out)
}

// ListClustersIncludingFailed is ListClusters with include_failed=true: the
// operator's view that also surfaces clusters whose create task failed.
func (c *Client) ListClustersIncludingFailed(ctx context.Context, poolID string, limit, offset int) (ClusterList, error) {
	q := url.Values{"include_failed": {"true"}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	var out ClusterList
	_, err := c.GetRaw(ctx, "/storage-pools/"+poolID+"/clusters", q, &out)
	return out, err
}

// EndpointCA is GET .../endpoints/{id}/ca. DocumentDBVerifiable is nil unless
// the endpoint reports a documentdb_address.
type EndpointCA struct {
	Required             bool   `json:"required"`
	DocumentDBVerifiable *bool  `json:"documentdb_verifiable"`
	CACrt                string `json:"ca_crt"`
	VerifyName           string `json:"verify_name"`
}

func (c *Client) GetEndpointCA(ctx context.Context, poolID, clusterID, branchID, endpointID string) (EndpointCA, error) {
	var out EndpointCA
	_, err := c.GetRaw(ctx, "/storage-pools/"+poolID+"/clusters/"+clusterID+"/branches/"+branchID+"/endpoints/"+endpointID+"/ca", nil, &out)
	return out, err
}

func (c *Client) GetUser(ctx context.Context, userID string) (User, error) {
	var out User
	_, err := c.GetRaw(ctx, "/users/"+userID, nil, &out)
	return out, err
}

// --- migrations (the Voyager surface, ac-voyager.md §4) ---

// MigrationSource is a source database the migration reads from. Password is
// write-only: it is accepted here and never returned by any read.
type MigrationSource struct {
	DBType   string   `json:"db_type"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username"`
	Password string   `json:"password,omitempty"`
	Database string   `json:"database"`
	Schemas  []string `json:"schemas"`
}

// MigrationConnectivity is the answer to a connectivity check. Unreachable is
// still HTTP 200, with Error set.
type MigrationConnectivity struct {
	Reachable     bool    `json:"reachable"`
	ServerVersion *string `json:"server_version"`
	SSL           *bool   `json:"ssl"`
	LatencyMS     *int64  `json:"latency_ms"`
	Error         *string `json:"error"`
}

// MigrationTarget is the amp endpoint a migration was provisioned onto.
type MigrationTarget struct {
	EndpointID string `json:"endpoint_id"`
	ClusterID  string `json:"cluster_id"`
	PoolID     string `json:"pool_id"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Database   string `json:"database"`
}

// MigrationRun is the latest attempt of one operation (assess, migrate, end).
type MigrationRun struct {
	Operation    string  `json:"operation"`
	Attempt      int     `json:"attempt"`
	Status       string  `json:"status"`
	TaskID       *string `json:"task_id"`
	ErrorSummary *string `json:"error_summary"`
}

// Migration is the derived-on-read status document the agent polls.
type Migration struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	State     string           `json:"state"`
	Detail    string           `json:"detail"`
	Source    MigrationSource  `json:"source"`
	Target    *MigrationTarget `json:"target"`
	Runs      []MigrationRun   `json:"runs"`
	Voyager   json.RawMessage  `json:"voyager"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// Migration states, in lifecycle order.
const (
	MigrationCreated            = "created"
	MigrationAssessing          = "assessing"
	MigrationAssessed           = "assessed"
	MigrationProvisioningTarget = "provisioning_target"
	MigrationTargetReady        = "target_ready"
	MigrationMigrating          = "migrating"
	MigrationCompleted          = "completed"
	MigrationEnding             = "ending"
	MigrationEnded              = "ended"
	MigrationFailed             = "failed"
)

// MigrationList is a page of migration summaries.
type MigrationList struct {
	Migrations []Migration `json:"migrations"`
	Total      int         `json:"total"`
	Count      int         `json:"count"`
}

type CreateMigrationRequest struct {
	Name   string          `json:"name"`
	Source MigrationSource `json:"source"`
}

type ProvisionMigrationTargetRequest struct {
	PoolID      string `json:"pool_id"`
	VCPU        int    `json:"vcpu,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
}

// CheckMigrationConnectivity dials the source from inside the cluster.
func (c *Client) CheckMigrationConnectivity(ctx context.Context, src MigrationSource) (MigrationConnectivity, error) {
	var out MigrationConnectivity
	_, _, err := c.PostRaw(ctx, "/migrations/connectivity-checks", struct {
		Source MigrationSource `json:"source"`
	}{src}, &out)
	return out, err
}

// CreateMigration inserts the migration row (synchronous 201, state created).
func (c *Client) CreateMigration(ctx context.Context, req CreateMigrationRequest) (Migration, error) {
	var out Migration
	_, _, err := c.PostRaw(ctx, "/migrations", req, &out)
	return out, err
}

// GetMigration reads the derived status, the surface to poll for progress.
func (c *Client) GetMigration(ctx context.Context, id string) (Migration, error) {
	var out Migration
	_, err := c.GetRaw(ctx, "/migrations/"+id, nil, &out)
	return out, err
}

// ListMigrations is one page of migrations.
func (c *Client) ListMigrations(ctx context.Context, limit, offset int) (MigrationList, error) {
	var out MigrationList
	_, err := c.GetRaw(ctx, "/migrations"+pageQuery(limit, offset), nil, &out)
	return out, err
}

// GetMigrationAssessment reads the parsed assessment report as raw JSON; the
// shape is Voyager's and the tests only need a few fields out of it.
func (c *Client) GetMigrationAssessment(ctx context.Context, id string) (json.RawMessage, error) {
	var out json.RawMessage
	_, err := c.GetRaw(ctx, "/migrations/"+id+"/assessment", nil, &out)
	return out, err
}

// StartMigrationAssessment launches the assess job (202 + the launch task).
func (c *Client) StartMigrationAssessment(ctx context.Context, id string) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.apiURL+"/migrations/"+id+"/assess", struct{}{})
}

// ProvisionMigrationTarget creates the target cluster + writer and attaches it.
func (c *Client) ProvisionMigrationTarget(ctx context.Context, id string, req ProvisionMigrationTargetRequest) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.apiURL+"/migrations/"+id+"/provision-target", req)
}

func (c *Client) StartMigration(ctx context.Context, id string) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.apiURL+"/migrations/"+id+"/migrate", struct{}{})
}

// EndMigration irreversibly cleans up Voyager metadata and export files.
func (c *Client) EndMigration(ctx context.Context, id string) (Task, string, error) {
	return c.async(ctx, http.MethodPost, c.apiURL+"/migrations/"+id+"/end", struct{}{})
}

func (c *Client) CancelTask(ctx context.Context, taskID string) (Task, error) {
	var out Task
	_, _, err := c.doJSON(ctx, http.MethodPost, c.apiURL+"/tasks/"+url.PathEscape(taskID)+"/cancel", nil, &out)
	return out, err
}

type ExtensionLayout struct {
	SharePath   string            `json:"share_path,omitempty"`
	LibPath     string            `json:"lib_path,omitempty"`
	LibraryPath []string          `json:"library_path,omitempty"`
	EnvPaths    map[string]string `json:"env_paths,omitempty"`
}

type Extension struct {
	Name           string           `json:"name"`
	Image          string           `json:"image"`
	Version        string           `json:"version,omitempty"`
	Description    string           `json:"description,omitempty"`
	Library        string           `json:"library,omitempty"`
	Preload        bool             `json:"preload"`
	Builtin        bool             `json:"builtin"`
	Layout         *ExtensionLayout `json:"layout,omitempty"`
	ABIExtraExempt []string         `json:"abi_extra_exempt,omitempty"`
}

type RegisterExtensionRequest struct {
	Image          string           `json:"image"`
	Version        string           `json:"version,omitempty"`
	Description    string           `json:"description,omitempty"`
	Library        string           `json:"library,omitempty"`
	Preload        bool             `json:"preload,omitempty"`
	Layout         *ExtensionLayout `json:"layout,omitempty"`
	ABIExtraExempt []string         `json:"abi_extra_exempt,omitempty"`
}

type ExtensionList struct {
	Extensions []Extension `json:"extensions"`
	Total      int         `json:"total"`
	Count      int         `json:"count"`
}

func (c *Client) ListExtensions(ctx context.Context) (ExtensionList, error) {
	var out ExtensionList
	_, err := c.GetRaw(ctx, "/extensions", nil, &out)
	return out, err
}

func (c *Client) GetExtension(ctx context.Context, name string) (Extension, error) {
	var out Extension
	_, err := c.GetRaw(ctx, "/extensions/"+url.PathEscape(name), nil, &out)
	return out, err
}

func (c *Client) RegisterExtension(ctx context.Context, name, userID string, req RegisterExtensionRequest) (Extension, error) {
	var out Extension
	_, _, err := c.doJSON(ctx, http.MethodPut, c.extensionURL(name, userID), req, &out)
	return out, err
}

func (c *Client) DeregisterExtension(ctx context.Context, name, userID string) error {
	_, _, err := c.doJSON(ctx, http.MethodDelete, c.extensionURL(name, userID), nil, nil)
	return err
}

func (c *Client) extensionURL(name, userID string) string {
	u := c.apiURL + "/extensions/" + url.PathEscape(name)
	if userID != "" {
		u += "?" + url.Values{"user_id": {userID}}.Encode()
	}
	return u
}
