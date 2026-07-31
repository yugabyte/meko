package yba

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// GCP provider + universe support. This mirrors the AWS path (provider.go /
// universe_create.go) for a YBA-managed universe standing up on Google Compute
// Engine VMs. Payload shapes mirror the stress framework's
// yugaware_app.add_gcp_provider (src/apps/yugaware/yugaware_app.py:2921) and the
// server model GCPCloudInfo
// (managed/.../models/helpers/provider/GCPCloudInfo.java): the provider carries
// the GCP project (gceProject), the service-account credentials JSON
// (gceApplicationCredentials, a STRING YBA parses), the existing VPC
// (destVpcId + useHostVPC + vpcType=EXISTING), regions→zones→subnets, and the
// SSH user/port. Like AWS it REUSES an existing SSH keypair (allAccessKeys +
// skipKeyValidateAndUpload) and NEVER mints one.
//
// The GCP universe userIntent (universe_create.go:gcpUserIntent) uses
// providerType=gcp with a GCE machine type as instanceType and a persistent-disk
// deviceInfo (storageType=Persistent, storageClass=standard — the stress
// framework's GCP choice, and Persistent is a valid GCP StorageType per
// managed/.../cloud/PublicCloudConstants.java). Unlike AWS there is no
// diskIops/throughput and no region ybImage (the DB image comes from the
// provider's image bundle / a YBA default), and unlike an API-created AWS
// provider YBA DOES auto-populate a GCP provider's instance-type catalog from
// the GCE API on create (managed/.../cloud/gcp/GCPInitializer.java), so the
// universe's machine type need NOT be registered explicitly.

// GCP provider/universe defaults. core/defaults_test.go asserts the embedded
// default.yaml agrees with these. Values mirror the stress framework's
// region_configs.py (CLOUD_DEFAULTS["gcp"] / GCP_REGIONS["us-west1"]) and its
// location_types MEDIUM_RAM GCP machine type.
const (
	// DefaultGCPProviderName is the idempotent identity — an existing gcp provider
	// with this name is reused rather than duplicated.
	DefaultGCPProviderName = "ampqa-gcp"
	// DefaultGCPProject is the shared dev GCP project (region_configs
	// CLOUD_DEFAULTS["gcp"].project_id).
	DefaultGCPProject = "yugabyte"
	// DefaultGCPNetwork / DefaultGCPSubnetwork are the shared dev VPC + us-west1
	// subnet (destVpcId + per-zone subnet).
	DefaultGCPNetwork    = "yugabyte-network"
	DefaultGCPSubnetwork = "subnet-us-west1"
	// DefaultGCPRegion / DefaultGCPZone are the shared dev region/zone. A single
	// zone keeps a cheap RF1 universe (and its reap) in one place.
	DefaultGCPRegion = "us-west1"
	DefaultGCPZone   = "us-west1-b"
	// DefaultGCPSSHUser / DefaultGCPSSHPort are the OS login YBA uses for
	// provisioned nodes (region_configs CLOUD_DEFAULTS["gcp"].username = centos).
	DefaultGCPSSHUser = "centos"
	DefaultGCPSSHPort = 22
	// DefaultGCPArch is x86_64 — GCP universes default to x86 (the stress
	// add_gcp_provider default), unlike the ARM AWS default.
	DefaultGCPArch = "x86"
	// DefaultGCPCredentialsSecret is the secret NAME whose Value resolves to a
	// PATH to the service-account JSON key file (the apps/cloud/gcp env-variant
	// convention, e.g. GCP_KEY=/path/to/sa.json). Its file CONTENTS are read into
	// gceApplicationCredentials; the PATH is reused for the reap's gcloud auth.
	DefaultGCPCredentialsSecret = "gcp_key"

	// DefaultGCPInstanceType is the n2-standard-4 GCE machine type (4 vCPU,
	// 16 GiB) — the GCP MEDIUM_RAM entry in the stress framework's
	// location_types, the analogue of the AWS m7g.xlarge default.
	DefaultGCPInstanceType = "n2-standard-4"
	// DefaultGCPNumNodes / DefaultGCPReplication are the cheap RF1 default (1
	// node); override for RF3.
	DefaultGCPNumNodes    = 1
	DefaultGCPReplication = 1
	// DefaultGCPVolumeSizeGB / DefaultGCPVolumeCount are the per-node persistent
	// disk size + count.
	DefaultGCPVolumeSizeGB = 100
	DefaultGCPVolumeCount  = 1
	// DefaultGCPStorageType is the GCE persistent-disk class YBA's deviceInfo
	// carries (a valid GCP StorageType). DefaultGCPStorageClass is the standard
	// disk class.
	DefaultGCPStorageType  = "Persistent"
	DefaultGCPStorageClass = "standard"
)

// GCPProviderSpec is a gcp-provider-create request in typed form. Values come
// from apps.yba.gcp.* (BuildGCPProviderSpec). CredentialsJSON and KeyPairContent
// are credential material — never logged, never recorded. CredentialsFile is the
// resolved PATH to the SA JSON, carried through so the teardown reap can auth
// gcloud with the same key; it is NOT part of the provider payload.
type GCPProviderSpec struct {
	Name    string // idempotent identity — an existing provider with this name is reused
	Project string // gceProject
	Network string // destVpcId (the existing VPC)

	SSHUser string
	SSHPort int
	Arch    string // "x86" | "arm" (mapped to YBA's Architecture enum in the image bundle)
	Zone    string // primary zone (used by the teardown reap, not the payload)

	// CredentialsJSON is the service-account key JSON (gceApplicationCredentials).
	// Sensitive. CredentialsFile is the PATH it was read from (reap gcloud auth).
	CredentialsJSON string
	CredentialsFile string

	// KeyPairName + KeyPairContent are an EXISTING SSH keypair reused via
	// allAccessKeys + skipKeyValidateAndUpload (YBA never mints one). Both empty →
	// no allAccessKeys block (YBA falls back to project/OS-login SSH keys). Empty
	// by default: no GCP-specific shared key exists yet — set both before a live
	// run. KeyPairContent is private-key material — never logged.
	KeyPairName    string
	KeyPairContent string

	// Image is an optional GCE image (name or family) for the provider's default
	// image bundle. Empty → no image bundle (YBA uses its default GCP image / a
	// live run sets it).
	Image string

	Regions []RegionSpec
}

// BuildGCPProviderSpec reads apps.yba.gcp.* into a GCPProviderSpec, resolving the
// service-account credentials by named secret (its Value is a PATH to the SA JSON
// — the apps/cloud/gcp env-variant convention — whose CONTENTS are read into
// gceApplicationCredentials) and reading the shared keypair's private-key
// CONTENTS off disk when configured. The credentials secret is mandatory: YBA
// stores them in the provider (no ambient fallback as there is for the local
// gcloud CLI), so a missing/failed secret is a hard error.
func BuildGCPProviderSpec(c ConfigGetter, resolve SecretResolver) (GCPProviderSpec, error) {
	if resolve == nil {
		return GCPProviderSpec{}, fmt.Errorf("yba gcp provider: no secret resolver available")
	}
	credSecret := c.GetString("apps.yba.gcp.credentials_secret", DefaultGCPCredentialsSecret)
	if credSecret == "" {
		return GCPProviderSpec{}, fmt.Errorf("apps.yba.gcp.credentials_secret is required (name of the secret resolving to the service-account JSON key path)")
	}
	credPath, err := resolve(credSecret)
	if err != nil {
		return GCPProviderSpec{}, fmt.Errorf("yba gcp credentials_secret %q: %w", credSecret, err)
	}
	credJSON, err := readKeyFile(credPath)
	if err != nil {
		return GCPProviderSpec{}, fmt.Errorf("yba gcp credentials_secret %q: read service-account key %q: %w", credSecret, credPath, err)
	}

	keyName := c.GetString("apps.yba.gcp.keypair_name", "")
	var keyContent string
	if keyFile := c.GetString("apps.yba.gcp.keypair_file", ""); keyFile != "" {
		keyContent, err = readKeyFile(keyFile)
		if err != nil {
			return GCPProviderSpec{}, fmt.Errorf("yba gcp keypair_file %q: %w", keyFile, err)
		}
	}

	regions, err := readGCPRegions(c)
	if err != nil {
		return GCPProviderSpec{}, err
	}

	return GCPProviderSpec{
		Name:            c.GetString("apps.yba.gcp.provider_name", DefaultGCPProviderName),
		Project:         c.GetString("apps.yba.gcp.project", DefaultGCPProject),
		Network:         c.GetString("apps.yba.gcp.network", DefaultGCPNetwork),
		SSHUser:         c.GetString("apps.yba.gcp.ssh_user", DefaultGCPSSHUser),
		SSHPort:         c.GetInt("apps.yba.gcp.ssh_port", DefaultGCPSSHPort),
		Arch:            c.GetString("apps.yba.gcp.arch", DefaultGCPArch),
		Zone:            c.GetString("apps.yba.gcp.zone", DefaultGCPZone),
		CredentialsJSON: credJSON,
		CredentialsFile: credPath,
		KeyPairName:     keyName,
		KeyPairContent:  keyContent,
		Image:           c.GetString("apps.yba.gcp.image", ""),
		Regions:         regions,
	}, nil
}

// readGCPRegions reads apps.yba.gcp.regions (a list of {code[,name], zones:[{name
// [,code],subnet}]}) into region specs, defaulting to the single shared us-west1
// region + us-west1-b zone (on the shared subnet) when none are configured.
func readGCPRegions(c ConfigGetter) ([]RegionSpec, error) {
	raw := c.GetMapSlice("apps.yba.gcp.regions", nil)
	if len(raw) == 0 {
		return []RegionSpec{{
			Code: DefaultGCPRegion,
			Zones: []ZoneSpec{{
				Name:   DefaultGCPZone,
				Subnet: DefaultGCPSubnetwork,
			}},
		}}, nil
	}
	out := make([]RegionSpec, 0, len(raw))
	for _, rm := range raw {
		r := RegionSpec{Code: mapStr(rm, "code")}
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

// AddGCPProvider plans an async GCP-provider create and returns its task
// reference (TaskRef.ResourceUUID is the new provider's UUID). The payload
// reuses the shared keypair when configured; it NEVER requests a new one.
func (c *Client) AddGCPProvider(ctx context.Context, spec GCPProviderSpec) (TaskRef, error) {
	if spec.Name == "" {
		return TaskRef{}, fmt.Errorf("add gcp provider: name is required")
	}
	if spec.Project == "" {
		return TaskRef{}, fmt.Errorf("add gcp provider %q: project is required", spec.Name)
	}
	if spec.CredentialsJSON == "" {
		return TaskRef{}, fmt.Errorf("add gcp provider %q: service-account credentials are required", spec.Name)
	}
	if len(spec.Regions) == 0 {
		return TaskRef{}, fmt.Errorf("add gcp provider %q: at least one region is required", spec.Name)
	}
	var out TaskRef
	url := c.custURL("/providers?validate=true&ignoreValidationErrors=true")
	if _, err := c.doJSON(ctx, http.MethodPost, url, gcpProviderPayload(spec), &out); err != nil {
		return TaskRef{}, fmt.Errorf("create gcp provider %q: %w", spec.Name, err)
	}
	return out, nil
}

// EnsureGCPProvider returns the UUID of a gcp provider named spec.Name, creating
// it (and waiting for the create task) only if none exists. Idempotent reuse is
// core infra hygiene (AGENTS.md §8): a second run reuses the provider instead of
// duplicating it. A provider-list failure is a hard error (not treated as
// "absent"), so a transient error never triggers a spurious create.
func (c *Client) EnsureGCPProvider(ctx context.Context, spec GCPProviderSpec) (string, error) {
	providers, err := c.GetProviders(ctx)
	if err != nil {
		return "", fmt.Errorf("ensure gcp provider %q: list providers: %w", spec.Name, err)
	}
	for _, p := range providers {
		if p.Name == spec.Name {
			c.log("yba: reusing existing provider %q (%s)", p.Name, p.UUID)
			return p.UUID, nil
		}
	}
	c.log("yba: creating gcp provider %q (project %s)", spec.Name, spec.Project)
	ref, err := c.AddGCPProvider(ctx, spec)
	if err != nil {
		return "", err
	}
	if _, err := c.WaitTask(ctx, ref.TaskUUID); err != nil {
		return "", fmt.Errorf("ensure gcp provider %q: %w", spec.Name, err)
	}
	if ref.ResourceUUID == "" {
		return "", fmt.Errorf("ensure gcp provider %q: create returned no provider uuid", spec.Name)
	}
	return ref.ResourceUUID, nil
}

// gcpProviderPayload builds the GCP provider-create body, mirroring the stress
// framework's add_gcp_provider shape: a gcp cloudInfo block (gceProject +
// gceApplicationCredentials + destVpcId + useHostVPC + vpcType=EXISTING),
// regions→zones→subnets, the SSH user/port, and — the hygiene-critical part —
// allAccessKeys carrying the EXISTING keypair (name + private-key content, with
// skipKeyValidateAndUpload) when one is configured, so YBA reuses the key and
// never mints one. An optional image bundle carries the GCE image.
func gcpProviderPayload(spec GCPProviderSpec) map[string]any {
	sshPort := spec.SSHPort
	if sshPort == 0 {
		sshPort = DefaultGCPSSHPort
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
				"code":   code,
				"name":   z.Name,
				"subnet": z.Subnet,
			})
		}
		regions = append(regions, map[string]any{
			"code":  r.Code,
			"name":  r.Code,
			"zones": zones,
		})
	}
	data := map[string]any{
		"code":    "gcp",
		"name":    spec.Name,
		"regions": regions,
		"details": map[string]any{
			"cloudInfo": map[string]any{
				"gcp": map[string]any{
					"gceProject":                spec.Project,
					"gceApplicationCredentials": spec.CredentialsJSON,
					"destVpcId":                 spec.Network,
					"useHostVPC":                true,
					"useHostCredentials":        false,
					"vpcType":                   "EXISTING",
				},
			},
			"airGapInstall": false,
			"ntpServers":    []any{},
			"setUpChrony":   true,
			"sshPort":       sshPort,
			"sshUser":       spec.SSHUser,
		},
	}
	if spec.KeyPairName != "" && spec.KeyPairContent != "" {
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
	if spec.Image != "" {
		data["imageBundles"] = []any{
			map[string]any{
				"name": spec.Name + "-bundle",
				"details": map[string]any{
					"arch":          ybaArch(spec.Arch),
					"sshUser":       spec.SSHUser,
					"sshPort":       sshPort,
					"globalYbImage": spec.Image,
				},
				"useAsDefault": true,
			},
		}
	}
	return data
}

// BuildGCPUniverseSpec reads the gcp universe shape from apps.yba.gcp.* (with the
// shared name_prefix, version, and instance tags from apps.yba.universe.*). The
// ProviderCode is gcp so BuildConfigureRequest emits the gcp userIntent. The
// release URL is derived with the GCP arch (x86 by default) so the imported DB
// tarball matches the GCE image.
func BuildGCPUniverseSpec(c ConfigGetter) UniverseSpec {
	version := c.GetString("apps.yba.universe.yb_version", "")
	if version == "" {
		version = c.GetString("versions.yugabytedb", "")
	}
	releaseURL := c.GetString("apps.yba.release_url", "")
	if releaseURL == "" {
		releaseURL = ReleaseTarballURL(version, c.GetString("apps.yba.gcp.arch", DefaultGCPArch))
	}
	return UniverseSpec{
		NamePrefix:        c.GetString("apps.yba.universe.name_prefix", DefaultUniverseNamePrefix),
		ProviderCode:      "gcp",
		Region:            c.GetString("apps.yba.gcp.region", DefaultGCPRegion),
		InstanceType:      c.GetString("apps.yba.gcp.instance_type", DefaultGCPInstanceType),
		NumNodes:          c.GetInt("apps.yba.gcp.num_nodes", DefaultGCPNumNodes),
		ReplicationFactor: c.GetInt("apps.yba.gcp.replication_factor", DefaultGCPReplication),
		YBSoftwareVersion: version,
		ReleaseURL:        releaseURL,
		VolumeSizeGB:      c.GetInt("apps.yba.gcp.volume_size_gb", DefaultGCPVolumeSizeGB),
		VolumeCount:       c.GetInt("apps.yba.gcp.volume_count", DefaultGCPVolumeCount),
		StorageType:       c.GetString("apps.yba.gcp.storage_type", DefaultGCPStorageType),
		InstanceTags:      instanceTagsFromConfig(c),
		EnableYbc:         c.GetBool("apps.yba.universe.enable_ybc", false),
	}
}

// gcpLabelInvalid matches any character not allowed in a GCP label key/value
// (only lowercase letters, digits, underscores, dashes).
var gcpLabelInvalid = regexp.MustCompile(`[^a-z0-9_-]`)

// sanitizeGCPLabels enforces GCP label rules on the userIntent instanceTags
// (GCE labels, unlike EC2 tags, allow only lowercase letters/digits/_/-, keys
// must start with a letter, max 63 chars) — mirrors the stress framework's
// region_configs.sanitize_gcp_labels so a universe's tags don't fail GCE label
// validation. Empty keys/values are dropped.
func sanitizeGCPLabels(tags map[string]string) map[string]string {
	clean := func(v string) string {
		s := gcpLabelInvalid.ReplaceAllString(strings.ToLower(v), "_")
		s = strings.Trim(s, "_-")
		if len(s) > 63 {
			s = s[:63]
		}
		return s
	}
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		key := clean(k)
		if key != "" && !(key[0] >= 'a' && key[0] <= 'z') {
			key = "x" + key
			if len(key) > 63 {
				key = key[:63]
			}
		}
		val := clean(v)
		if key != "" && val != "" {
			out[key] = val
		}
	}
	return out
}
