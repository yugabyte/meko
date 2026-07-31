package yba

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Provider/universe creation. This is the ampqa equivalent of the stress
// framework's yugaware_app.add_aws_provider + create_universe, narrowed to what
// the framework needs and following YBA's own two-call universe flow.
//
// Infra hygiene (AGENTS.md §8) is the load-bearing property of provider
// creation: the AWS provider REUSES the shared yb-dev-aws-2 keypair by passing
// the existing key name + its private-key CONTENTS under allAccessKeys with
// skipKeyValidateAndUpload:true, so YBA never auto-mints a new AWS keypair.
// There is deliberately NO create-key-pair path anywhere in this package.

// Provider-creation defaults. The AWS values match apps/cloud/aws so a provider
// and a bare-VM host describe the same shared dev VPC; the embedded default.yaml
// carries the same values and core/defaults_test.go asserts they agree.
const (
	// DefaultProviderName is the reused/idempotent provider name — an existing
	// provider with this name is reused rather than duplicated.
	DefaultProviderName = "ampqa-aws"
	// DefaultProviderSSHUser is the OS login YBA uses for provisioned nodes.
	DefaultProviderSSHUser = "ec2-user"
	// DefaultArch is aarch64 (ARM) — the ampqa default architecture.
	DefaultArch = "arm"
	// DefaultKeyPairName / DefaultKeyPairFile are the SHARED keypair reused for
	// every provider — never a freshly minted one.
	DefaultKeyPairName = "yb-dev-aws-2"
	DefaultKeyPairFile = "~/.ssh/yb-dev-aws-2.pem"
	// DefaultAccessKeySecret / DefaultSecretKeySecret are the secret NAMES (not
	// values) resolved for the AWS access/secret credentials.
	DefaultAccessKeySecret = "aws_access_key"
	DefaultSecretKeySecret = "aws_secret_key"

	// Universe-spec defaults. InstanceType is the ARM m7g.xlarge that
	// location_types' MEDIUM_RAM maps to for AWS under Architecture=ARM, matching
	// apps/cloud/aws's DefaultInstanceType.
	DefaultUniverseNamePrefix = "ampqa"
	DefaultNumNodes           = 3
	DefaultReplicationFactor  = 3
	DefaultInstanceType       = "m7g.xlarge"
	DefaultUniverseRegion     = "us-west-2"
	DefaultVolumeSizeGB       = 100
	DefaultVolumeCount        = 1
	DefaultStorageType        = "GP3"
	// DefaultInstanceNumCores/MemGB describe the default m7g.xlarge (4 vCPU,
	// 16 GiB) registered on the provider so YBA can resolve the instance type at
	// universe-create — YBA does not auto-populate ARM/Graviton types.
	DefaultInstanceNumCores = 4
	DefaultInstanceMemGB    = 16
	// GP3 requires an explicit diskIops/throughput (YBA 400s without them); these
	// are the AWS GP3 baseline (3000 IOPS, 125 MB/s), matching the stress
	// framework's volume_iops/volume_throughput defaults.
	DefaultDiskIops   = 3000
	DefaultThroughput = 125
)

// ConfigGetter is the subset of *core.Config the spec builders read. Taking an
// interface keeps apps/yba free of a core import (the apps/* convention, as in
// apps/cloud/aws).
type ConfigGetter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetBool(key string, def bool) bool
	GetMap(key string, def map[string]any) map[string]any
	GetMapSlice(key string, def []map[string]any) []map[string]any
}

// SecretResolver maps a secret NAME to its resolved value; it matches
// core.Secrets.Value. Taking a func keeps this package free of a core import.
type SecretResolver func(name string) (string, error)

// NewFromConfig builds a Client from apps.yba.* — base_url and customer_uuid
// (both required), and the API token resolved from the secret NAMED by
// apps.yba.api_token_secret. opts inject the http.Client (TLS policy) and a
// masked log sink. This is the entry point env/universe (next wave) calls to
// reach a YBA for provider/universe creation.
func NewFromConfig(c ConfigGetter, resolve SecretResolver, opts ...Option) (*Client, error) {
	baseURL := c.GetString("apps.yba.base_url", "")
	if baseURL == "" {
		return nil, fmt.Errorf("apps.yba.base_url is required")
	}
	customer := c.GetString("apps.yba.customer_uuid", "")
	if customer == "" {
		return nil, fmt.Errorf("apps.yba.customer_uuid is required")
	}
	tokenSecret := c.GetString("apps.yba.api_token_secret", "")
	if tokenSecret == "" {
		return nil, fmt.Errorf("apps.yba.api_token_secret is required (name of the secret holding the YBA API token)")
	}
	if resolve == nil {
		return nil, fmt.Errorf("apps.yba: no secret resolver available to resolve api_token_secret %q", tokenSecret)
	}
	token, err := resolve(tokenSecret)
	if err != nil {
		return nil, fmt.Errorf("apps.yba api_token_secret %q: %w", tokenSecret, err)
	}
	return New(baseURL, customer, token, opts...), nil
}

// --- provider spec ---

// ProviderSpec is a cloud-provider-create request in typed form. Only AWS is
// implemented; the Cloud field selects the builder so GCP/Azure can be added
// later without changing callers.
type ProviderSpec struct {
	Cloud string // "aws" (only implemented cloud)
	Name  string // idempotent identity — an existing provider with this name is reused

	SSHUser string
	Arch    string // "arm" | "x86"

	// KeyPairName + KeyPairContent are the SHARED existing keypair: passing both
	// (with skipKeyValidateAndUpload) makes YBA REUSE the key instead of minting
	// one. KeyPairContent is private-key material — never logged, never recorded.
	KeyPairName    string
	KeyPairContent string

	// AccessKey/SecretKey are the static AWS credentials, already RESOLVED from
	// named secrets by the caller (BuildAWSProviderSpec). Baked into the provider
	// payload's cloudInfo — never logged.
	AccessKey string
	SecretKey string

	Regions []RegionSpec

	// K8s carries the kubernetes-provider payload when Cloud is "kubernetes"/"k8s"
	// (the k8s form is shaped differently from AWS — see K8sProviderSpec). nil for
	// AWS. Callers usually reach k8s via EnsureK8sProvider directly; this field
	// lets the generic AddProvider dispatcher handle it too.
	K8s *K8sProviderSpec

	// GCP carries the gcp-provider payload when Cloud is "gcp" (the gcp form is
	// shaped differently from AWS — see GCPProviderSpec). nil for AWS. Callers
	// usually reach gcp via EnsureGCPProvider directly; this field lets the generic
	// AddProvider dispatcher handle it too.
	GCP *GCPProviderSpec
}

// RegionSpec is one provider region and its zones/subnets.
type RegionSpec struct {
	Code            string
	SecurityGroupID string
	VPCID           string
	AMI             string
	Zones           []ZoneSpec
}

// ZoneSpec is one availability zone in a region.
type ZoneSpec struct {
	Name   string
	Code   string // "" → Name
	Subnet string
}

// BuildAWSProviderSpec reads apps.yba.aws.* into a ProviderSpec, resolving the
// AWS access/secret credentials by named secret and reading the shared keypair's
// private-key CONTENTS off disk. Credentials are mandatory: YBA stores them in
// the provider (there is no ambient-chain fallback as there is for the local aws
// CLI), so a missing/failed secret is a hard error rather than a silent skip.
func BuildAWSProviderSpec(c ConfigGetter, resolve SecretResolver) (ProviderSpec, error) {
	if resolve == nil {
		return ProviderSpec{}, fmt.Errorf("yba provider: no secret resolver available")
	}
	accessName := c.GetString("apps.yba.aws.access_key_secret", DefaultAccessKeySecret)
	secretName := c.GetString("apps.yba.aws.secret_key_secret", DefaultSecretKeySecret)
	accessKey, err := resolve(accessName)
	if err != nil {
		return ProviderSpec{}, fmt.Errorf("yba aws access_key_secret %q: %w", accessName, err)
	}
	secretKey, err := resolve(secretName)
	if err != nil {
		return ProviderSpec{}, fmt.Errorf("yba aws secret_key_secret %q: %w", secretName, err)
	}

	keyName := c.GetString("apps.yba.aws.keypair_name", DefaultKeyPairName)
	keyFile := c.GetString("apps.yba.aws.keypair_file", DefaultKeyPairFile)
	keyContent, err := readKeyFile(keyFile)
	if err != nil {
		return ProviderSpec{}, fmt.Errorf("yba aws keypair_file %q: %w", keyFile, err)
	}

	regions, err := readAWSRegions(c)
	if err != nil {
		return ProviderSpec{}, err
	}

	return ProviderSpec{
		Cloud:          "aws",
		Name:           c.GetString("apps.yba.aws.provider_name", DefaultProviderName),
		SSHUser:        c.GetString("apps.yba.aws.ssh_user", DefaultProviderSSHUser),
		Arch:           c.GetString("apps.yba.aws.arch", DefaultArch),
		KeyPairName:    keyName,
		KeyPairContent: keyContent,
		AccessKey:      accessKey,
		SecretKey:      secretKey,
		Regions:        regions,
	}, nil
}

func readAWSRegions(c ConfigGetter) ([]RegionSpec, error) {
	raw := c.GetMapSlice("apps.yba.aws.regions", nil)
	if len(raw) == 0 {
		return nil, fmt.Errorf("apps.yba.aws.regions is empty; at least one region is required to create a provider")
	}
	out := make([]RegionSpec, 0, len(raw))
	for _, rm := range raw {
		r := RegionSpec{
			Code:            mapStr(rm, "code"),
			SecurityGroupID: mapStr(rm, "security_group_id"),
			VPCID:           mapStr(rm, "vpc_id"),
			AMI:             mapStr(rm, "ami"),
		}
		for _, zm := range subMaps(rm["zones"]) {
			r.Zones = append(r.Zones, ZoneSpec{
				Name:   mapStr(zm, "name"),
				Code:   mapStr(zm, "code"),
				Subnet: mapStr(zm, "subnet"),
			})
		}
		out = append(out, r)
	}
	return out, nil
}

// --- provider create ---

// AddProvider dispatches on spec.Cloud. AWS, GCP and kubernetes are implemented;
// Azure returns a clear not-implemented error rather than a half-built payload.
// The kubernetes case creates synchronously (no task), so the returned TaskRef
// carries only the new provider's ResourceUUID.
func (c *Client) AddProvider(ctx context.Context, spec ProviderSpec) (TaskRef, error) {
	switch spec.Cloud {
	case "aws":
		return c.AddAWSProvider(ctx, spec)
	case "gcp":
		if spec.GCP == nil {
			return TaskRef{}, fmt.Errorf("yba provider cloud %q requires spec.GCP (gcp uses GCPProviderSpec)", spec.Cloud)
		}
		return c.AddGCPProvider(ctx, *spec.GCP)
	case "kubernetes", "k8s":
		if spec.K8s == nil {
			return TaskRef{}, fmt.Errorf("yba provider cloud %q requires spec.K8s (kubernetes uses K8sProviderSpec)", spec.Cloud)
		}
		uuid, err := c.AddK8sProvider(ctx, *spec.K8s)
		if err != nil {
			return TaskRef{}, err
		}
		return TaskRef{ResourceUUID: uuid}, nil
	case "azu", "azure":
		return TaskRef{}, fmt.Errorf("yba provider cloud %q not implemented (only aws, gcp, kubernetes)", spec.Cloud)
	default:
		return TaskRef{}, fmt.Errorf("yba provider cloud %q unknown (only aws, gcp, kubernetes)", spec.Cloud)
	}
}

// AddAWSProvider plans an async AWS-provider create and returns its task
// reference (TaskRef.ResourceUUID is the new provider's UUID). The payload
// reuses the shared keypair; it NEVER requests a new one.
func (c *Client) AddAWSProvider(ctx context.Context, spec ProviderSpec) (TaskRef, error) {
	if spec.Name == "" {
		return TaskRef{}, fmt.Errorf("add aws provider: name is required")
	}
	if len(spec.Regions) == 0 {
		return TaskRef{}, fmt.Errorf("add aws provider %q: at least one region is required", spec.Name)
	}
	var out TaskRef
	// validate=true asks YBA to validate creds/subnets; ignoreValidationErrors
	// keeps a soft validation warning from blocking creation (the stress-app
	// choice), so a transient validation hiccup doesn't fail provisioning.
	url := c.custURL("/providers?validate=true&ignoreValidationErrors=true")
	_, err := c.doJSON(ctx, http.MethodPost, url, awsProviderPayload(spec), &out)
	if err != nil {
		return TaskRef{}, fmt.Errorf("create aws provider %q: %w", spec.Name, err)
	}
	return out, nil
}

// EnsureAWSProvider returns the UUID of an AWS provider named spec.Name,
// creating it (and waiting for the create task) only if none exists. Idempotent
// reuse is core infra hygiene (AGENTS.md §8): a second run against the same YBA
// reuses the provider instead of duplicating it. A provider-list failure is a
// hard error (not treated as "absent"), so a transient error never triggers a
// spurious create.
func (c *Client) EnsureAWSProvider(ctx context.Context, spec ProviderSpec) (string, error) {
	providers, err := c.GetProviders(ctx)
	if err != nil {
		return "", fmt.Errorf("ensure aws provider %q: list providers: %w", spec.Name, err)
	}
	for _, p := range providers {
		if p.Name == spec.Name {
			c.log("yba: reusing existing provider %q (%s)", p.Name, p.UUID)
			return p.UUID, nil
		}
	}
	c.log("yba: creating aws provider %q (reusing keypair %q)", spec.Name, spec.KeyPairName)
	ref, err := c.AddAWSProvider(ctx, spec)
	if err != nil {
		return "", err
	}
	if _, err := c.WaitTask(ctx, ref.TaskUUID); err != nil {
		return "", fmt.Errorf("ensure aws provider %q: %w", spec.Name, err)
	}
	if ref.ResourceUUID == "" {
		return "", fmt.Errorf("ensure aws provider %q: create returned no provider uuid", spec.Name)
	}
	return ref.ResourceUUID, nil
}

// AccessKey is one of a provider's SSH access keys. Its idKey.keyCode is the
// keypair YBA launches nodes with; the universe userIntent must carry it as
// accessKeyCode or node provisioning gets KeyName=None from the cloud SDK.
type AccessKey struct {
	IDKey struct {
		KeyCode      string `json:"keyCode"`
		ProviderUUID string `json:"providerUUID"`
	} `json:"idKey"`
}

// ProviderAccessKeyCode returns the first access-key code of a provider (GET
// …/providers/{uuid}/access_keys). The provider-create task registers the key,
// so it is present once EnsureAWSProvider has waited on that task.
func (c *Client) ProviderAccessKeyCode(ctx context.Context, providerUUID string) (string, error) {
	var keys []AccessKey
	if _, err := c.doJSON(ctx, http.MethodGet, c.custURL("/providers/"+providerUUID+"/access_keys"), nil, &keys); err != nil {
		return "", fmt.Errorf("list access keys for provider %s: %w", providerUUID, err)
	}
	for _, k := range keys {
		if code := strings.TrimSpace(k.IDKey.KeyCode); code != "" {
			return code, nil
		}
	}
	return "", fmt.Errorf("provider %s has no access key", providerUUID)
}

// InstanceTypeSpec registers one instance type on a provider. For an
// API-created AWS provider YBA does NOT auto-populate its instance-type catalog
// (ARM/Graviton types in particular), so the universe's chosen type must be
// added explicitly or universe-create NPEs on a null InstanceType. Mirrors the
// stress framework's add_instance_type payload.
type InstanceTypeSpec struct {
	Code      string // e.g. "m7g.xlarge"
	NumCores  int
	MemSizeGB int
	Arch      string // "arm"/"x86" shorthand; mapped to YBA's Architecture enum
	MountPath string // instance-type volumeDetailsList mount ("" → /home/ec2-user)
}

func instanceTypePayload(providerUUID string, s InstanceTypeSpec) map[string]any {
	mount := s.MountPath
	if mount == "" {
		mount = "/home/ec2-user"
	}
	return map[string]any{
		"idKey": map[string]any{
			"instanceTypeCode": s.Code,
			"providerUuid":     providerUUID,
		},
		"instanceTypeDetails": map[string]any{
			"tenancy": "Shared",
			"volumeDetailsList": []any{
				map[string]any{"mountPath": mount, "volumeSizeGB": 0, "volumeType": "EBS"},
			},
			"arch": ybaArch(s.Arch),
		},
		"memSizeGB": s.MemSizeGB,
		"numCores":  s.NumCores,
	}
}

// AddInstanceType registers an instance type on a provider (POST
// …/providers/{uuid}/instance_types) so YBA can resolve it at universe-create.
func (c *Client) AddInstanceType(ctx context.Context, providerUUID string, s InstanceTypeSpec) error {
	if s.Code == "" {
		return fmt.Errorf("add instance type: empty instance type code")
	}
	if providerUUID == "" {
		return fmt.Errorf("add instance type %q: empty provider uuid", s.Code)
	}
	url := c.custURL("/providers/" + providerUUID + "/instance_types")
	var out map[string]any
	if _, err := c.doJSON(ctx, http.MethodPost, url, instanceTypePayload(providerUUID, s), &out); err != nil {
		return fmt.Errorf("add instance type %q on provider %s: %w", s.Code, providerUUID, err)
	}
	return nil
}

// ybaArch maps the config's arch shorthand ("arm"/"x86") to YBA's Architecture
// enum, whose only accepted values are "aarch64" and "x86_64" — YBA 400s on any
// other string. An already-canonical or unknown value is passed through.
func ybaArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "arm", "arm64", "aarch64":
		return "aarch64"
	case "x86", "x86_64", "amd64":
		return "x86_64"
	default:
		return arch
	}
}

// awsProviderPayload builds the AWS provider-create body, mirroring the stress
// framework's add_aws_provider shape: regions → zones → subnets, an
// aws.awsAccessKeyID/Secret cloudInfo block, and — the hygiene-critical part —
// allAccessKeys carrying the EXISTING keypair name + private-key content with
// skipKeyValidateAndUpload so YBA reuses the key and never mints one.
func awsProviderPayload(spec ProviderSpec) map[string]any {
	arch := spec.Arch
	if arch == "" {
		arch = DefaultArch
	}
	regions := make([]any, 0, len(spec.Regions))
	for _, r := range spec.Regions {
		zones := make([]any, 0, len(r.Zones))
		for _, z := range r.Zones {
			code := z.Code
			if code == "" {
				code = z.Name
			}
			zones = append(zones, map[string]any{
				"name":   z.Name,
				"code":   code,
				"subnet": z.Subnet,
			})
		}
		regions = append(regions, map[string]any{
			"code":  r.Code,
			"zones": zones,
			"details": map[string]any{
				"cloudInfo": map[string]any{
					"aws": map[string]any{
						"arch":            ybaArch(arch),
						"securityGroupId": r.SecurityGroupID,
						"vnet":            r.VPCID,
						"ybImage":         r.AMI,
					},
				},
			},
		})
	}
	data := map[string]any{
		"code":    "aws",
		"name":    spec.Name,
		"regions": regions,
		"details": map[string]any{
			"airGapInstall": false,
			"cloudInfo": map[string]any{
				"aws": map[string]any{
					"awsAccessKeyID":     spec.AccessKey,
					"awsAccessKeySecret": spec.SecretKey,
				},
			},
			"ntpServers":  []any{},
			"setUpChrony": true,
			"sshPort":     22,
			"sshUser":     spec.SSHUser,
		},
	}
	if spec.KeyPairName != "" {
		data["allAccessKeys"] = []any{
			map[string]any{
				"keyInfo": map[string]any{
					"sshPrivateKeyContent":     spec.KeyPairContent,
					"keyPairName":              spec.KeyPairName,
					"skipKeyValidateAndUpload": true,
				},
			},
		}
	}
	return data
}

// --- provider regions (read) ---

// Region is a provider region with its zones (read for universe placement).
type Region struct {
	UUID  string `json:"uuid"`
	Code  string `json:"code"`
	Name  string `json:"name"`
	Zones []Zone `json:"zones"`
}

// Zone is one availability zone of a provider region.
type Zone struct {
	UUID string `json:"uuid"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// GetProviderRegions lists a provider's regions (and their zone UUIDs) — the
// source of the region UUIDs a universe's userIntent.regionList needs.
func (c *Client) GetProviderRegions(ctx context.Context, providerUUID string) ([]Region, error) {
	var out []Region
	_, err := c.doJSON(ctx, http.MethodGet, c.custURL("/providers/"+providerUUID+"/regions"), nil, &out)
	return out, err
}

// RegionUUIDsForCodes resolves the UUIDs of the provider's regions whose codes
// are in codes (all regions when codes is empty). An unmatched code is an error
// — a universe placed in a region the provider doesn't have would fail opaquely
// later.
func (c *Client) RegionUUIDsForCodes(ctx context.Context, providerUUID string, codes ...string) ([]string, error) {
	regions, err := c.GetProviderRegions(ctx, providerUUID)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]string, len(regions))
	all := make([]string, 0, len(regions))
	for _, r := range regions {
		byCode[r.Code] = r.UUID
		all = append(all, r.UUID)
	}
	if len(codes) == 0 {
		return all, nil
	}
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		uuid, ok := byCode[code]
		if !ok {
			return nil, fmt.Errorf("provider %s has no region %q", providerUUID, code)
		}
		out = append(out, uuid)
	}
	return out, nil
}

// --- helpers ---

func readKeyFile(path string) (string, error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// expandHome expands a leading ~ to the user's home directory (mirrors
// core.secret.go's helper; kept local so apps/yba imports no core).
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func mapStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		if v != nil {
			return fmt.Sprint(v)
		}
	}
	return ""
}

func subMaps(v any) []map[string]any {
	s, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(s))
	for _, e := range s {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
