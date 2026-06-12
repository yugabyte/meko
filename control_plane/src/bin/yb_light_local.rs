//!
//! `yb_light_local` is an executable that can be used to create a local
//! ybLight environment, for testing purposes. The local environment is
//! quite different from the cloud environment with Kubernetes, but it
//! easier to work with locally. The python tests in `test_runner`
//! rely on `yb_light_local` to set up the environment for each test.
//!
use std::borrow::Cow;
use std::collections::{BTreeSet, HashMap};
use std::fs::File;
use std::path::PathBuf;
use std::process::exit;
use std::str::FromStr;
use std::time::Duration;

use anyhow::{Context, Result, anyhow, bail};
use clap::Parser;
use compute_api::requests::ComputeClaimsScope;
use compute_api::spec::{ComputeMode, PageserverProtocol};
use control_plane::endpoint::{ComputeControlPlane, EndpointTerminateMode};
use control_plane::endpoint::local_pageserver_conf_to_conn_info;
use control_plane::endpoint_storage::{ENDPOINT_STORAGE_DEFAULT_ADDR, EndpointStorage};
use control_plane::local_env;
use control_plane::local_env::{
    EndpointStorageConf, InitForceMode, LocalEnv, PageServerConf, YbLightLocalInitConf,
    YbLightLocalInitPageserverConf, YbLightStorageControllerConf, SafekeeperConf,
};
use control_plane::pageserver::PageServerNode;
use control_plane::safekeeper::SafekeeperNode;
use control_plane::storage_controller::{
    YbLightStorageControllerStartArgs, YbLightStorageControllerStopArgs, StorageController,
};
use nix::fcntl::{Flock, FlockArg};
use pageserver_api::config::{
    DEFAULT_GRPC_LISTEN_PORT as DEFAULT_PAGESERVER_GRPC_PORT,
    DEFAULT_HTTP_LISTEN_PORT as DEFAULT_PAGESERVER_HTTP_PORT,
    DEFAULT_PG_LISTEN_PORT as DEFAULT_PAGESERVER_PG_PORT,
};
use pageserver_api::controller_api::{NodeAvailabilityWrapper, PlacementPolicy};
use pageserver_api::models::TimelineInfo;
use postgres_backend::AuthType;
use safekeeper_api::membership::{SafekeeperGeneration, SafekeeperId};
use safekeeper_api::{
    DEFAULT_HTTP_LISTEN_PORT as DEFAULT_SAFEKEEPER_HTTP_PORT,
    DEFAULT_PG_LISTEN_PORT as DEFAULT_SAFEKEEPER_PG_PORT, PgMajorVersion, PgVersionId,
};
use tokio::task::JoinSet;
use utils::auth::{Claims, Scope};
use utils::id::{NodeId, TenantId, TenantTimelineId, TimelineId};
use utils::lsn::Lsn;
use utils::project_git_version;

// Default id of a safekeeper node, if not specified on the command line.
const DEFAULT_SAFEKEEPER_ID: NodeId = NodeId(1);
const DEFAULT_PAGESERVER_ID: NodeId = NodeId(1);
const DEFAULT_BRANCH_NAME: &str = "main";
project_git_version!(GIT_VERSION);

#[allow(dead_code)]
const DEFAULT_PG_VERSION: PgMajorVersion = PgMajorVersion::PG17;
const DEFAULT_PG_VERSION_NUM: &str = "17";

const DEFAULT_PAGESERVER_CONTROL_PLANE_API: &str = "http://127.0.0.1:1234/upcall/v1/";

const DEFAULT_YB_HOST: &str = "127.0.0.1";
const DEFAULT_YB_PORT: u16 = 5433;
const DEFAULT_YB_USER: &str = "yugabyte";
const DEFAULT_YB_DBNAME: &str = "yb_storage";

/// Build a `YbStoreConfig` TOML inline table targeting `table_name` in the
/// dev cluster's YB. Consumed verbatim as the pageserver `remote_storage`
/// override and the safekeeper `--wal-yb-config` argument.
fn default_yb_store_inline_table(table_name: &str) -> String {
    format!(
        "{{ host = '{DEFAULT_YB_HOST}', port = {DEFAULT_YB_PORT}, \
         user = '{DEFAULT_YB_USER}', dbname = '{DEFAULT_YB_DBNAME}', \
         table_name = '{table_name}' }}"
    )
}

/// `storage_controller.database_url` for the dev cluster's YB.
fn default_storage_controller_database_url() -> String {
    format!("postgresql://{DEFAULT_YB_USER}@{DEFAULT_YB_HOST}:{DEFAULT_YB_PORT}/{DEFAULT_YB_DBNAME}")
}

/// ybLight CLI.
#[derive(clap::Parser)]
#[command(version = GIT_VERSION, name = "ybLight CLI")]
struct Cli {
    #[command(subcommand)]
    command: YbLightLocalCmd,
}

#[derive(clap::Subcommand)]
enum YbLightLocalCmd {
    Init(InitCmdArgs),

    #[command(subcommand)]
    Tenant(TenantCmd),
    #[command(subcommand)]
    Timeline(TimelineCmd),
    #[command(subcommand)]
    Pageserver(PageserverCmd),
    #[command(subcommand)]
    #[clap(alias = "storage_controller")]
    StorageController(StorageControllerCmd),
    #[command(subcommand)]
    Safekeeper(SafekeeperCmd),
    #[command(subcommand)]
    EndpointStorage(EndpointStorageCmd),
    #[command(subcommand)]
    Endpoint(EndpointCmd),
    #[command(subcommand)]
    Mappings(MappingsCmd),

    Start(StartCmdArgs),
    Stop(StopCmdArgs),
}

/// Initialize a new ybLight repository, preparing configs for services to start with.
#[derive(clap::Args)]
struct InitCmdArgs {
    /// How many pageservers to create (default 1).
    #[clap(long)]
    num_pageservers: Option<u16>,

    #[clap(long)]
    config: Option<PathBuf>,

    /// Force initialization even if the repository is not empty.
    #[clap(long, default_value = "must-not-exist")]
    #[arg(value_parser)]
    force: InitForceMode,
}

/// Start pageserver and safekeepers.
#[derive(clap::Args)]
struct StartCmdArgs {
    // 120s default: on a freshly-created YB-backed `yb_storage` DB,
    // storage_controller's diesel migrations + our own schema bootstrap
    // take ~60-90s end-to-end. The previous 10s default fired before
    // migrations finished and the pageserver's first call to
    // storage_ctl saw "relation storage_controller.timelines does not
    // exist". See tasks/4.k_yb-light-local-start-wedge.md.
    #[clap(long = "start-timeout", default_value = "120s")]
    timeout: humantime::Duration,
}

/// Stop pageserver and safekeepers.
#[derive(clap::Args)]
struct StopCmdArgs {
    #[arg(value_enum)]
    #[clap(long, default_value_t = StopMode::Fast)]
    mode: StopMode,
}

#[derive(Clone, Copy, clap::ValueEnum)]
enum StopMode {
    Fast,
    Immediate,
}

/// Manage tenants.
#[derive(clap::Subcommand)]
enum TenantCmd {
    List,
    Create(TenantCreateCmdArgs),
    SetDefault(TenantSetDefaultCmdArgs),
    Config(TenantConfigCmdArgs),
    Import(TenantImportCmdArgs),
}

#[derive(clap::Args)]
struct TenantCreateCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long = "tenant-id")]
    tenant_id: Option<TenantId>,

    /// Use a specific timeline id when creating a tenant and its initial timeline.
    #[clap(long)]
    timeline_id: Option<TimelineId>,

    #[clap(short = 'c')]
    config: Vec<String>,

    /// Postgres version to use for the initial timeline.
    #[arg(default_value = DEFAULT_PG_VERSION_NUM)]
    #[clap(long)]
    pg_version: PgMajorVersion,

    /// Use this tenant in future CLI commands where tenant_id is needed, but not specified.
    #[clap(long)]
    set_default: bool,

    /// Number of shards in the new tenant.
    #[clap(long)]
    #[arg(default_value_t = 0)]
    shard_count: u8,
    /// Sharding stripe size in pages.
    #[clap(long)]
    shard_stripe_size: Option<u32>,

    /// Placement policy shards in this tenant.
    #[clap(long)]
    #[arg(value_parser = parse_placement_policy)]
    placement_policy: Option<PlacementPolicy>,
}

fn parse_placement_policy(s: &str) -> anyhow::Result<PlacementPolicy> {
    Ok(serde_json::from_str::<PlacementPolicy>(s)?)
}

/// Set a particular tenant as default in future CLI commands where tenant_id is needed, but not
/// specified.
#[derive(clap::Args)]
struct TenantSetDefaultCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long = "tenant-id")]
    tenant_id: TenantId,
}

#[derive(clap::Args)]
struct TenantConfigCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long = "tenant-id")]
    tenant_id: Option<TenantId>,

    #[clap(short = 'c')]
    config: Vec<String>,
}

/// Import a tenant that is present in remote storage, and create branches for its timelines.
#[derive(clap::Args)]
struct TenantImportCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long = "tenant-id")]
    tenant_id: TenantId,
}

/// Manage timelines.
#[derive(clap::Subcommand)]
enum TimelineCmd {
    List(TimelineListCmdArgs),
    Branch(TimelineBranchCmdArgs),
    Create(TimelineCreateCmdArgs),
    Import(TimelineImportCmdArgs),
}

/// List all timelines available to this pageserver.
#[derive(clap::Args)]
struct TimelineListCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long = "tenant-id")]
    tenant_shard_id: Option<TenantId>,
}

/// Create a new timeline, branching off from another timeline.
#[derive(clap::Args)]
struct TimelineBranchCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long = "tenant-id")]
    tenant_id: Option<TenantId>,
    /// New timeline's ID, as a 32-byte hexadecimal string.
    #[clap(long)]
    timeline_id: Option<TimelineId>,
    /// Human-readable alias for the new timeline.
    #[clap(long)]
    branch_name: String,
    /// Use last Lsn of another timeline (and its data) as base when creating the new timeline. The
    /// timeline gets resolved by its branch name.
    #[clap(long)]
    ancestor_branch_name: Option<String>,
    /// When using another timeline as base, use a specific Lsn in it instead of the latest one.
    #[clap(long)]
    ancestor_start_lsn: Option<Lsn>,
}

/// Create a new blank timeline.
#[derive(clap::Args)]
struct TimelineCreateCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long = "tenant-id")]
    tenant_id: Option<TenantId>,
    /// New timeline's ID, as a 32-byte hexadecimal string.
    #[clap(long)]
    timeline_id: Option<TimelineId>,
    /// Human-readable alias for the new timeline.
    #[clap(long)]
    branch_name: String,

    /// Postgres version.
    #[arg(default_value = DEFAULT_PG_VERSION_NUM)]
    #[clap(long)]
    pg_version: PgMajorVersion,
}

/// Import a timeline from a basebackup directory.
#[derive(clap::Args)]
struct TimelineImportCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long = "tenant-id")]
    tenant_id: Option<TenantId>,
    /// New timeline's ID, as a 32-byte hexadecimal string.
    #[clap(long)]
    timeline_id: TimelineId,
    /// Human-readable alias for the new timeline.
    #[clap(long)]
    branch_name: String,
    /// Basebackup tarfile to import.
    #[clap(long)]
    base_tarfile: PathBuf,
    /// LSN the basebackup starts at.
    #[clap(long)]
    base_lsn: Lsn,
    /// WAL to add after base.
    #[clap(long)]
    wal_tarfile: Option<PathBuf>,
    /// LSN the basebackup ends at.
    #[clap(long)]
    end_lsn: Option<Lsn>,

    /// Postgres version of the basebackup being imported.
    #[arg(default_value = DEFAULT_PG_VERSION_NUM)]
    #[clap(long)]
    pg_version: PgMajorVersion,
}

/// Manage pageservers.
#[derive(clap::Subcommand)]
enum PageserverCmd {
    Status(PageserverStatusCmdArgs),
    Start(PageserverStartCmdArgs),
    Stop(PageserverStopCmdArgs),
    Restart(PageserverRestartCmdArgs),
}

/// Show status of a local pageserver.
#[derive(clap::Args)]
struct PageserverStatusCmdArgs {
    /// Pageserver ID.
    #[clap(long = "id")]
    pageserver_id: Option<NodeId>,
}

/// Start local pageserver.
#[derive(clap::Args)]
struct PageserverStartCmdArgs {
    /// Pageserver ID.
    #[clap(long = "id")]
    pageserver_id: Option<NodeId>,
    /// Timeout until we fail the command.
    #[clap(short = 't', long)]
    #[arg(default_value = "10s")]
    start_timeout: humantime::Duration,
}

/// Stop local pageserver.
#[derive(clap::Args)]
struct PageserverStopCmdArgs {
    /// Pageserver ID.
    #[clap(long = "id")]
    pageserver_id: Option<NodeId>,
    /// If 'immediate', don't flush repository data at shutdown
    #[clap(short = 'm')]
    #[arg(value_enum, default_value = "fast")]
    stop_mode: StopMode,
}

/// Restart local pageserver.
#[derive(clap::Args)]
struct PageserverRestartCmdArgs {
    /// Pageserver ID.
    #[clap(long = "id")]
    pageserver_id: Option<NodeId>,
    /// Timeout until we fail the command.
    #[clap(short = 't', long)]
    #[arg(default_value = "10s")]
    start_timeout: humantime::Duration,
}

/// Manage storage controller.
#[derive(clap::Subcommand)]
enum StorageControllerCmd {
    Start(StorageControllerStartCmdArgs),
    Stop(StorageControllerStopCmdArgs),
}

/// Start storage controller.
#[derive(clap::Args)]
struct StorageControllerStartCmdArgs {
    /// Timeout until we fail the command.
    #[clap(short = 't', long)]
    #[arg(default_value = "10s")]
    start_timeout: humantime::Duration,
    /// Identifier used to distinguish storage controller instances.
    #[clap(long)]
    #[arg(default_value_t = 1)]
    instance_id: u8,
    /// Base port for the storage controller instance identified by instance-id (defaults to
    /// pageserver cplane api).
    #[clap(long)]
    base_port: Option<u16>,
}

/// Stop storage controller.
#[derive(clap::Args)]
struct StorageControllerStopCmdArgs {
    /// If 'immediate', don't flush repository data at shutdown
    #[clap(short = 'm')]
    #[arg(value_enum, default_value = "fast")]
    stop_mode: StopMode,
    /// Identifier used to distinguish storage controller instances.
    #[clap(long)]
    #[arg(default_value_t = 1)]
    instance_id: u8,
}

/// Manage safekeepers.
#[derive(clap::Subcommand)]
enum SafekeeperCmd {
    Start(SafekeeperStartCmdArgs),
    Stop(SafekeeperStopCmdArgs),
    Restart(SafekeeperRestartCmdArgs),
}

/// Manage object storage.
#[derive(clap::Subcommand)]
enum EndpointStorageCmd {
    Start(EndpointStorageStartCmd),
    Stop(EndpointStorageStopCmd),
}

/// Start object storage.
#[derive(clap::Args)]
struct EndpointStorageStartCmd {
    /// Timeout until we fail the command.
    #[clap(short = 't', long)]
    #[arg(default_value = "10s")]
    start_timeout: humantime::Duration,
}

/// Stop object storage.
#[derive(clap::Args)]
struct EndpointStorageStopCmd {
    /// If 'immediate', don't flush repository data on shutdown.
    #[clap(short = 'm')]
    #[arg(value_enum, default_value = "fast")]
    stop_mode: StopMode,
}

/// Start local safekeeper.
#[derive(clap::Args)]
struct SafekeeperStartCmdArgs {
    /// Safekeeper ID.
    #[arg(default_value_t = NodeId(1))]
    id: NodeId,

    /// Additional safekeeper invocation options, e.g. -e=--http-auth-public-key-path=foo.
    #[clap(short = 'e', long = "safekeeper-extra-opt")]
    extra_opt: Vec<String>,

    /// Timeout until we fail the command.
    #[clap(short = 't', long)]
    #[arg(default_value = "10s")]
    start_timeout: humantime::Duration,
}

/// Stop local safekeeper.
#[derive(clap::Args)]
struct SafekeeperStopCmdArgs {
    /// Safekeeper ID.
    #[arg(default_value_t = NodeId(1))]
    id: NodeId,

    /// If 'immediate', don't flush repository data on shutdown.
    #[arg(value_enum, default_value = "fast")]
    #[clap(short = 'm')]
    stop_mode: StopMode,
}

/// Restart local safekeeper.
#[derive(clap::Args)]
struct SafekeeperRestartCmdArgs {
    /// Safekeeper ID.
    #[arg(default_value_t = NodeId(1))]
    id: NodeId,

    /// If 'immediate', don't flush repository data on shutdown.
    #[arg(value_enum, default_value = "fast")]
    #[clap(short = 'm')]
    stop_mode: StopMode,

    /// Additional safekeeper invocation options, e.g. -e=--http-auth-public-key-path=foo.
    #[clap(short = 'e', long = "safekeeper-extra-opt")]
    extra_opt: Vec<String>,

    /// Timeout until we fail the command.
    #[clap(short = 't', long)]
    #[arg(default_value = "10s")]
    start_timeout: humantime::Duration,
}

/// Manage Postgres instances.
#[derive(clap::Subcommand)]
enum EndpointCmd {
    List(EndpointListCmdArgs),
    Create(EndpointCreateCmdArgs),
    Start(EndpointStartCmdArgs),
    Reconfigure(EndpointReconfigureCmdArgs),
    RefreshConfiguration(EndpointRefreshConfigurationArgs),
    Stop(EndpointStopCmdArgs),
    UpdatePageservers(EndpointUpdatePageserversCmdArgs),
    GenerateJwt(EndpointGenerateJwtCmdArgs),
}

/// List endpoints.
#[derive(clap::Args)]
struct EndpointListCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long = "tenant-id")]
    tenant_shard_id: Option<TenantId>,
}

/// Create a compute endpoint.
#[derive(clap::Args)]
struct EndpointCreateCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long = "tenant-id")]
    tenant_id: Option<TenantId>,
    /// Postgres endpoint ID.
    endpoint_id: Option<String>,
    /// Name of the branch the endpoint will run on.
    #[clap(long)]
    branch_name: Option<String>,
    /// Specify LSN on the timeline to start from. By default, end of the timeline would be used.
    #[clap(long)]
    lsn: Option<Lsn>,
    #[clap(long)]
    pg_port: Option<u16>,
    #[clap(long, alias = "http-port")]
    external_http_port: Option<u16>,
    #[clap(long)]
    internal_http_port: Option<u16>,
    #[clap(long = "pageserver-id")]
    endpoint_pageserver_id: Option<NodeId>,

    /// Don't do basebackup, create endpoint directory with only config files.
    #[clap(long, action = clap::ArgAction::Set, default_value_t = false)]
    config_only: bool,

    /// Postgres version.
    #[arg(default_value = DEFAULT_PG_VERSION_NUM)]
    #[clap(long)]
    pg_version: PgMajorVersion,

    /// Use gRPC to communicate with Pageservers, by generating grpc:// connstrings.
    ///
    /// Specified on creation such that it's retained across reconfiguration and restarts.
    ///
    /// NB: not yet supported by computes.
    #[clap(long)]
    grpc: bool,

    /// If set, the node will be a hot replica on the specified timeline.
    #[clap(long, action = clap::ArgAction::Set, default_value_t = false)]
    hot_standby: bool,
    /// If set, will set up the catalog for neon_superuser.
    #[clap(long)]
    update_catalog: bool,
    /// Allow multiple primary endpoints running on the same branch. Shouldn't be used normally, but
    /// useful for tests.
    #[clap(long)]
    allow_multiple: bool,

    /// Name of the privileged role for the endpoint.
    // Only allow changing it on creation.
    #[clap(long)]
    privileged_role_name: Option<String>,

    /// `shared_buffers` value written into the generated postgresql.conf.
    /// Defaults to `1MB` (upstream-compatible smoke-test sizing). Pass
    /// e.g. `--shared-buffers=32GB` when running on a large host. Can
    /// also be edited later in `endpoints/<id>/endpoint.json`.
    #[clap(long)]
    shared_buffers: Option<String>,

    /// Enables Neon's Local File Cache (LFC) on the compute by setting
    /// both `neon.max_file_cache_size` (postmaster) and
    /// `neon.file_cache_size_limit` (SIGHUP) to this value. Pass a
    /// Postgres size literal, e.g. `--lfc-size=8GB`. Without this, LFC
    /// is disabled and every shared_buffers miss is a getpage RPC.
    #[clap(long)]
    lfc_size: Option<String>,

    /// Repeatable: `--pg-setting NAME=VALUE` appends arbitrary settings
    /// to the generated postgresql.conf. Use for knobs that don't have a
    /// dedicated flag — `--pg-setting max_connections=200
    /// --pg-setting effective_cache_size=96GB`.
    #[clap(long = "pg-setting", value_parser = parse_pg_setting)]
    pg_settings: Vec<(String, String)>,
}

fn parse_pg_setting(s: &str) -> Result<(String, String), String> {
    let (k, v) = s
        .split_once('=')
        .ok_or_else(|| format!("expected NAME=VALUE, got '{s}'"))?;
    let k = k.trim();
    let v = v.trim();
    if k.is_empty() {
        return Err(format!("empty setting name in '{s}'"));
    }
    Ok((k.to_string(), v.to_string()))
}

/// Start Postgres. If the endpoint doesn't exist yet, it is created.
#[derive(clap::Args)]
struct EndpointStartCmdArgs {
    /// Postgres endpoint ID.
    endpoint_id: String,
    /// Pageserver ID.
    #[clap(long = "pageserver-id")]
    endpoint_pageserver_id: Option<NodeId>,
    /// Safekeepers membership generation to prefix neon.safekeepers with.
    #[clap(long)]
    safekeepers_generation: Option<u32>,
    /// List of safekeepers endpoint will talk to.
    #[clap(long)]
    safekeepers: Option<String>,
    /// Configure the remote extensions storage proxy gateway URL to request for extensions.
    #[clap(long, alias = "remote-ext-config")]
    remote_ext_base_url: Option<String>,
    /// If set, will create test user `user` and `neondb` database. Requires `update-catalog = true`
    #[clap(long)]
    create_test_user: bool,
    /// Allow multiple primary endpoints running on the same branch. Shouldn't be used normally, but
    /// useful for tests.
    #[clap(long)]
    allow_multiple: bool,
    /// Timeout until we fail the command.
    #[clap(short = 't', long, value_parser= humantime::parse_duration)]
    #[arg(default_value = "90s")]
    start_timeout: Duration,

    /// Download LFC cache from endpoint storage on endpoint startup
    #[clap(long, default_value = "false")]
    autoprewarm: bool,

    /// Upload LFC cache to endpoint storage periodically
    #[clap(long)]
    offload_lfc_interval_seconds: Option<std::num::NonZeroU64>,

    /// Run in development mode, skipping VM-specific operations like process termination
    #[clap(long, action = clap::ArgAction::SetTrue)]
    dev: bool,
}

/// Reconfigure an endpoint.
#[derive(clap::Args)]
struct EndpointReconfigureCmdArgs {
    /// Tenant id. Represented as a hexadecimal string 32 symbols length
    #[clap(long = "tenant-id")]
    tenant_id: Option<TenantId>,
    /// Postgres endpoint ID.
    endpoint_id: String,
    /// Pageserver ID.
    #[clap(long = "pageserver-id")]
    endpoint_pageserver_id: Option<NodeId>,
    #[clap(long)]
    safekeepers: Option<String>,
}

/// Refresh the endpoint's configuration by forcing it reload it's spec
#[derive(clap::Args)]
struct EndpointRefreshConfigurationArgs {
    /// Postgres endpoint id
    endpoint_id: String,
}

/// Stop an endpoint.
#[derive(clap::Args)]
struct EndpointStopCmdArgs {
    /// Postgres endpoint ID.
    endpoint_id: String,
    /// Also delete data directory (now optional, should be default in future).
    #[clap(long)]
    destroy: bool,

    /// Postgres shutdown mode, passed to `pg_ctl -m <mode>`.
    #[clap(long)]
    #[clap(default_value = "fast")]
    mode: EndpointTerminateMode,
}

/// Update the pageservers in the spec file of the compute endpoint
#[derive(clap::Args)]
struct EndpointUpdatePageserversCmdArgs {
    /// Postgres endpoint id
    endpoint_id: String,

    /// Specified pageserver id
    #[clap(short = 'p', long)]
    pageserver_id: Option<NodeId>,
}

/// Generate a JWT for an endpoint.
#[derive(clap::Args)]
struct EndpointGenerateJwtCmdArgs {
    /// Postgres endpoint ID.
    endpoint_id: String,
    /// Scope to generate the JWT with.
    #[clap(short = 's', long, value_parser = ComputeClaimsScope::from_str)]
    scope: Option<ComputeClaimsScope>,
}

/// Manage yb_light_local branch name mappings.
#[derive(clap::Subcommand)]
enum MappingsCmd {
    Map(MappingsMapCmdArgs),
}

/// Create new mapping which cannot exist already.
#[derive(clap::Args)]
struct MappingsMapCmdArgs {
    /// Tenant ID, as a 32-byte hexadecimal string.
    #[clap(long)]
    tenant_id: TenantId,
    /// Timeline ID, as a 32-byte hexadecimal string.
    #[clap(long)]
    timeline_id: TimelineId,
    /// Branch name to give to the timeline.
    #[clap(long)]
    branch_name: String,
}

///
/// Timelines tree element used as a value in the HashMap.
///
struct TimelineTreeEl {
    /// `TimelineInfo` received from the `pageserver` via the `timeline_list` http API call.
    pub info: TimelineInfo,
    /// Name, recovered from ybLight config mappings
    pub name: Option<String>,
    /// Holds all direct children of this timeline referenced using `timeline_id`.
    pub children: BTreeSet<TimelineId>,
}

/// A flock-based guard over the yb_light_local repository directory
struct RepoLock {
    _file: Flock<File>,
}

impl RepoLock {
    fn new() -> Result<Self> {
        let path = local_env::base_path();
        let repo_dir = File::open(&path).with_context(|| {
            format!(
                "no yb_light_local repo at {} \u{2014} run `yb_light_local init` first",
                path.display()
            )
        })?;
        match Flock::lock(repo_dir, FlockArg::LockExclusive) {
            Ok(f) => Ok(Self { _file: f }),
            Err((_, e)) => Err(e).context("flock error"),
        }
    }
}

// Main entry point for the 'yb_light_local' CLI utility
//
// This utility helps to manage ybLight installation. That includes following:
//   * Management of local postgres installations running on top of the
//     pageserver.
//   * Providing CLI api to the pageserver
//   * TODO: export/import to/from usual postgres
fn main() -> Result<()> {
    let cli = Cli::parse();

    // Check for 'ybLight init' command first.
    let (subcommand_result, _lock) = if let YbLightLocalCmd::Init(args) = cli.command {
        (handle_init(&args).map(|env| Some(Cow::Owned(env))), None)
    } else {
        // This tool uses a collection of simple files to store its state, and consequently
        // it is not generally safe to run multiple commands concurrently.  Rather than expect
        // all callers to know this, use a lock file to protect against concurrent execution.
        let _repo_lock = RepoLock::new()?;

        // all other commands need an existing config
        let env = LocalEnv::load_config(&local_env::base_path()).context("Error loading config")?;
        let original_env = env.clone();
        let env = Box::leak(Box::new(env));
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();

        let subcommand_result = match cli.command {
            YbLightLocalCmd::Init(_) => unreachable!("init was handled earlier already"),
            YbLightLocalCmd::Start(args) => rt.block_on(handle_start_all(&args, env)),
            YbLightLocalCmd::Stop(args) => rt.block_on(handle_stop_all(&args, env)),
            YbLightLocalCmd::Tenant(subcmd) => rt.block_on(handle_tenant(&subcmd, env)),
            YbLightLocalCmd::Timeline(subcmd) => rt.block_on(handle_timeline(&subcmd, env)),
            YbLightLocalCmd::Pageserver(subcmd) => rt.block_on(handle_pageserver(&subcmd, env)),
            YbLightLocalCmd::StorageController(subcmd) => {
                rt.block_on(handle_storage_controller(&subcmd, env))
            }
            YbLightLocalCmd::Safekeeper(subcmd) => rt.block_on(handle_safekeeper(&subcmd, env)),
            YbLightLocalCmd::EndpointStorage(subcmd) => {
                rt.block_on(handle_endpoint_storage(&subcmd, env))
            }
            YbLightLocalCmd::Endpoint(subcmd) => rt.block_on(handle_endpoint(&subcmd, env)),
            YbLightLocalCmd::Mappings(subcmd) => rt.block_on(handle_mappings(&subcmd, env)),
        };

        let subcommand_result = if &original_env != env {
            subcommand_result.map(|()| Some(Cow::Borrowed(env)))
        } else {
            subcommand_result.map(|()| None)
        };
        (subcommand_result, Some(_repo_lock))
    };

    match subcommand_result {
        Ok(Some(updated_env)) => updated_env.persist_config()?,
        Ok(None) => (),
        Err(e) => {
            eprintln!("command failed: {e:?}");
            exit(1);
        }
    }
    Ok(())
}

///
/// Prints timelines list as a tree-like structure.
///
fn print_timelines_tree(
    timelines: Vec<TimelineInfo>,
    mut timeline_name_mappings: HashMap<TenantTimelineId, String>,
) -> Result<()> {
    let mut timelines_hash = timelines
        .iter()
        .map(|t| {
            (
                t.timeline_id,
                TimelineTreeEl {
                    info: t.clone(),
                    children: BTreeSet::new(),
                    name: timeline_name_mappings
                        .remove(&TenantTimelineId::new(t.tenant_id, t.timeline_id)),
                },
            )
        })
        .collect::<HashMap<_, _>>();

    // Memorize all direct children of each timeline.
    for timeline in timelines.iter() {
        if let Some(ancestor_timeline_id) = timeline.ancestor_timeline_id {
            timelines_hash
                .get_mut(&ancestor_timeline_id)
                .context("missing timeline info in the HashMap")?
                .children
                .insert(timeline.timeline_id);
        }
    }

    for timeline in timelines_hash.values() {
        // Start with root local timelines (no ancestors) first.
        if timeline.info.ancestor_timeline_id.is_none() {
            print_timeline(0, &Vec::from([true]), timeline, &timelines_hash)?;
        }
    }

    Ok(())
}

///
/// Recursively prints timeline info with all its children.
///
fn print_timeline(
    nesting_level: usize,
    is_last: &[bool],
    timeline: &TimelineTreeEl,
    timelines: &HashMap<TimelineId, TimelineTreeEl>,
) -> Result<()> {
    if nesting_level > 0 {
        let ancestor_lsn = match timeline.info.ancestor_lsn {
            Some(lsn) => lsn.to_string(),
            None => "Unknown Lsn".to_string(),
        };

        let mut br_sym = "┣━";

        // Draw each nesting padding with proper style
        // depending on whether its timeline ended or not.
        if nesting_level > 1 {
            for l in &is_last[1..is_last.len() - 1] {
                if *l {
                    print!("   ");
                } else {
                    print!("┃  ");
                }
            }
        }

        // We are the last in this sub-timeline
        if *is_last.last().unwrap() {
            br_sym = "┗━";
        }

        print!("{br_sym} @{ancestor_lsn}: ");
    }

    // Finally print a timeline id and name with new line
    println!(
        "{} [{}]",
        timeline.name.as_deref().unwrap_or("_no_name_"),
        timeline.info.timeline_id
    );

    let len = timeline.children.len();
    let mut i: usize = 0;
    let mut is_last_new = Vec::from(is_last);
    is_last_new.push(false);

    for child in &timeline.children {
        i += 1;

        // Mark that the last padding is the end of the timeline
        if i == len {
            if let Some(last) = is_last_new.last_mut() {
                *last = true;
            }
        }

        print_timeline(
            nesting_level + 1,
            &is_last_new,
            timelines
                .get(child)
                .context("missing timeline info in the HashMap")?,
            timelines,
        )?;
    }

    Ok(())
}

/// Helper function to get tenant id from an optional --tenant_id option or from the config file
fn get_tenant_id(
    tenant_id_arg: Option<TenantId>,
    env: &local_env::LocalEnv,
) -> anyhow::Result<TenantId> {
    if let Some(tenant_id_from_arguments) = tenant_id_arg {
        Ok(tenant_id_from_arguments)
    } else if let Some(default_id) = env.default_tenant_id {
        Ok(default_id)
    } else {
        anyhow::bail!("No tenant id. Use --tenant-id, or set a default tenant");
    }
}

/// Helper function to get tenant-shard ID from an optional --tenant_id option or from the config file,
/// for commands that accept a shard suffix
fn get_tenant_shard_id(
    tenant_shard_id_arg: Option<TenantId>,
    env: &local_env::LocalEnv,
) -> anyhow::Result<TenantId> {
    if let Some(tenant_id_from_arguments) = tenant_shard_id_arg {
        Ok(tenant_id_from_arguments)
    } else if let Some(default_id) = env.default_tenant_id {
        Ok(TenantId::unsharded(default_id))
    } else {
        anyhow::bail!("No tenant shard id. Use --tenant-id, or set a default tenant");
    }
}

fn handle_init(args: &InitCmdArgs) -> anyhow::Result<LocalEnv> {
    // Create the in-memory `LocalEnv` that we'd normally load from disk in `load_config`.
    let init_conf: YbLightLocalInitConf = if let Some(config_path) = &args.config {
        // User (likely the Python test suite) provided a description of the environment.
        if args.num_pageservers.is_some() {
            bail!(
                "Cannot specify both --num-pageservers and --config, use key `pageservers` in the --config file instead"
            );
        }
        // load and parse the file
        let contents = std::fs::read_to_string(config_path).with_context(|| {
            format!(
                "Could not read configuration file '{}'",
                config_path.display()
            )
        })?;
        toml_edit::de::from_str(&contents)?
    } else {
        // User (likely interactive) did not provide a description of the environment, give them the default
        YbLightLocalInitConf {
            control_plane_api: Some(DEFAULT_PAGESERVER_CONTROL_PLANE_API.parse().unwrap()),
            safekeepers: vec![SafekeeperConf {
                id: DEFAULT_SAFEKEEPER_ID,
                pg_port: DEFAULT_SAFEKEEPER_PG_PORT,
                http_port: DEFAULT_SAFEKEEPER_HTTP_PORT,
                // Mirror live WAL into yb_storage.public.wals.
                wal_yb_config: Some(default_yb_store_inline_table("wals")),
                ..Default::default()
            }],
            pageservers: (0..args.num_pageservers.unwrap_or(1))
                .map(|i| {
                    let pageserver_id = NodeId(DEFAULT_PAGESERVER_ID.0 + i as u64);
                    let pg_port = DEFAULT_PAGESERVER_PG_PORT + i;
                    let http_port = DEFAULT_PAGESERVER_HTTP_PORT + i;
                    let grpc_port = DEFAULT_PAGESERVER_GRPC_PORT + i;
                    YbLightLocalInitPageserverConf {
                        id: pageserver_id,
                        listen_pg_addr: format!("127.0.0.1:{pg_port}"),
                        listen_http_addr: format!("127.0.0.1:{http_port}"),
                        listen_https_addr: None,
                        listen_grpc_addr: Some(format!("127.0.0.1:{grpc_port}")),
                        pg_auth_type: AuthType::Trust,
                        http_auth_type: AuthType::Trust,
                        grpc_auth_type: AuthType::Trust,
                        // Relfile/layer objects live in yb_storage.public.blobs.
                        remote_storage: Some(default_yb_store_inline_table("blobs")),
                        other: Default::default(),
                        // Typical developer machines use disks with slow fsync, and we don't care
                        // about data integrity: disable disk syncs.
                        no_sync: true,
                    }
                })
                .collect(),
            endpoint_storage: EndpointStorageConf {
                listen_addr: ENDPOINT_STORAGE_DEFAULT_ADDR,
                remote_storage_json: None,
                endpoint_id: String::new(),
            },
            pg_distrib_dir: None,
            yb_light_distrib_dir: None,
            default_tenant_id: TenantId::from_array(std::array::from_fn(|_| 0)),
            storage_controller: Some(YbLightStorageControllerConf {
                // Provision the storage_controller + yb_storage DBs in YB.
                database_url: Some(default_storage_controller_database_url()),
                ..Default::default()
            }),
            control_plane_hooks_api: None,
            use_local_tls: false,
        }
    };

    LocalEnv::init(init_conf, &args.force)
        .context("materialize initial yb_light_local environment on disk")?;
    Ok(LocalEnv::load_config(&local_env::base_path())
        .expect("freshly written config should be loadable"))
}

/// `host:port` → `host:(port + delta)`.
fn bump_addr_port(addr: &str, delta: u16) -> anyhow::Result<String> {
    let (host, port) = addr
        .rsplit_once(':')
        .with_context(|| format!("no port in address '{addr}'"))?;
    let port: u16 = port
        .parse()
        .with_context(|| format!("bad port in address '{addr}'"))?;
    Ok(format!("{host}:{}", port + delta))
}

/// Allocate the conf for a pageserver to be provisioned under `endpoint_id`.
/// The node id is the next free one (the lowest-id template's id when none
/// are provisioned yet, so the first/main pageserver reuses the template's
/// id and ports); listen ports derive from the lowest-id template by a
/// stable per-id offset. Auth/no_sync come from the template. This is the
/// single allocation path for all pageservers, main and branch alike.
fn allocate_pageserver_conf(env: &LocalEnv, endpoint_id: &str) -> anyhow::Result<PageServerConf> {
    let template = env
        .pageserver_templates
        .iter()
        .min_by_key(|p| p.id.0)
        .context("no pageserver template configured")?;
    let new_id = match env.pageservers.iter().map(|p| p.id.0).max() {
        Some(max) => max + 1,
        None => template.id.0,
    };
    let delta = u16::try_from((new_id - template.id.0) * 2).context("pageserver id overflow")?;
    Ok(PageServerConf {
        id: NodeId(new_id),
        listen_pg_addr: bump_addr_port(&template.listen_pg_addr, delta)?,
        listen_http_addr: bump_addr_port(&template.listen_http_addr, delta)?,
        listen_https_addr: template
            .listen_https_addr
            .as_deref()
            .map(|a| bump_addr_port(a, delta))
            .transpose()?,
        listen_grpc_addr: template
            .listen_grpc_addr
            .as_deref()
            .map(|a| bump_addr_port(a, delta))
            .transpose()?,
        pg_auth_type: template.pg_auth_type,
        http_auth_type: template.http_auth_type,
        grpc_auth_type: template.grpc_auth_type,
        no_sync: template.no_sync,
        tenant_id: None,
        timeline_id: None,
        data_dir: env.pageserver_data_dir(endpoint_id),
    })
}

/// Build the full `YbLightLocalInitPageserverConf` (carrying
/// `remote_storage` + other passthrough keys) for the first/main
/// pageserver, by cloning the lowest-id template and stamping in the
/// allocated id/ports/auth. Used to write the bootstrap pageserver.toml;
/// branch pageservers instead clone their ancestor's toml.
fn pageserver_init_conf_for(
    env: &LocalEnv,
    allocated: &PageServerConf,
) -> anyhow::Result<YbLightLocalInitPageserverConf> {
    let template = env
        .pageserver_templates
        .iter()
        .min_by_key(|p| p.id.0)
        .context("no pageserver template configured")?;
    Ok(YbLightLocalInitPageserverConf {
        id: allocated.id,
        listen_pg_addr: allocated.listen_pg_addr.clone(),
        listen_http_addr: allocated.listen_http_addr.clone(),
        listen_https_addr: allocated.listen_https_addr.clone(),
        listen_grpc_addr: allocated.listen_grpc_addr.clone(),
        pg_auth_type: allocated.pg_auth_type,
        http_auth_type: allocated.http_auth_type,
        grpc_auth_type: allocated.grpc_auth_type,
        no_sync: allocated.no_sync,
        remote_storage: template.remote_storage.clone(),
        other: template.other.clone(),
    })
}

/// Allocate the conf for a safekeeper to be provisioned under `endpoint_id`,
/// dedicated to `timeline_id`. Like [`allocate_pageserver_conf`], the first
/// safekeeper reuses the lowest-id template's id/ports; subsequent ones get
/// the next free id and a per-id port offset.
fn allocate_safekeeper_conf(
    env: &LocalEnv,
    endpoint_id: &str,
    timeline_id: TimelineId,
) -> anyhow::Result<SafekeeperConf> {
    let template = env
        .safekeeper_templates
        .iter()
        .min_by_key(|s| s.id.0)
        .context("no safekeeper template configured")?;
    let new_id = match env.safekeepers.iter().map(|s| s.id.0).max() {
        Some(max) => max + 1,
        None => template.id.0,
    };
    let delta = u16::try_from((new_id - template.id.0) * 2).context("safekeeper id overflow")?;
    Ok(SafekeeperConf {
        id: NodeId(new_id),
        pg_port: template.pg_port + delta,
        pg_tenant_only_port: template.pg_tenant_only_port.map(|p| p + delta),
        http_port: template.http_port + delta,
        https_port: template.https_port.map(|p| p + delta),
        backup_threads: template.backup_threads,
        auth_enabled: template.auth_enabled,
        listen_addr: template.listen_addr.clone(),
        wal_yb_config: template.wal_yb_config.clone(),
        wal_cache_size_mib: template.wal_cache_size_mib,
        timeline_id: Some(timeline_id),
        endpoint_id: endpoint_id.to_string(),
    })
}

/// Allocate the conf for the endpoint_storage instance to be provisioned
/// under `endpoint_id`. The listen port is the lowest free port of the form
/// `template_base + 2k` not already taken by another instance — so the
/// first endpoint reuses the template's port and later ones step by 2.
/// `remote_storage_json` is cloned from the template.
fn allocate_endpoint_storage_conf(env: &LocalEnv, endpoint_id: &str) -> EndpointStorageConf {
    let base = env.endpoint_storage.listen_addr.port();
    let used: std::collections::HashSet<u16> = env
        .endpoint_storages
        .iter()
        .map(|e| e.listen_addr.port())
        .collect();
    let mut port = base;
    while used.contains(&port) {
        port += 2;
    }
    let mut listen_addr = env.endpoint_storage.listen_addr;
    listen_addr.set_port(port);
    EndpointStorageConf {
        listen_addr,
        remote_storage_json: env.endpoint_storage.remote_storage_json.clone(),
        endpoint_id: endpoint_id.to_string(),
    }
}

/// Provision and start the endpoint_storage instance dedicated to
/// `endpoint_id` under `endpoints/<endpoint_id>/endpoint_storage`, then
/// record it in the env. One instance per endpoint, alongside the
/// co-located PS/SK pair. Tolerates an old yb-light binary without the
/// `endpoint_storage` executable (compat tests): skip rather than fail.
async fn provision_endpoint_storage(env: &mut LocalEnv, endpoint_id: &str) -> anyhow::Result<()> {
    let conf = allocate_endpoint_storage_conf(env, endpoint_id);
    let storage = EndpointStorage::from_conf(env, &conf);
    if !storage.bin.exists() {
        eprintln!(
            "{} binary not found; skipping endpoint_storage provisioning \
             (ignore if this is a compatibility test)",
            storage.bin
        );
        return Ok(());
    }
    println!(
        "Provisioning endpoint_storage ({}) for endpoint '{endpoint_id}'",
        conf.listen_addr
    );
    storage.init().context("endpoint_storage init")?;
    storage
        .start(&std::time::Duration::from_secs(30))
        .await
        .context("start endpoint_storage")?;
    env.endpoint_storages.push(conf);
    Ok(())
}

/// The ancestor timeline's durable WAL end (`flush_lsn`), read from its
/// safekeeper. This is the authoritative "everything committed so far"
/// position; the ancestor pageserver's `last_record_lsn` only reflects
/// what its walreceiver has ingested and may lag this by seconds.
async fn ancestor_durable_lsn(
    env: &LocalEnv,
    tenant_id: TenantId,
    timeline_id: TimelineId,
) -> anyhow::Result<Lsn> {
    let sk_conf = env
        .safekeepers_for_timeline(timeline_id)
        .into_iter()
        .next()
        .with_context(|| format!("no safekeeper serves timeline {timeline_id}"))?
        .clone();
    let sk = SafekeeperNode::from_env(env, &sk_conf);
    let status: safekeeper_api::models::TimelineStatus = sk
        .http_client
        .timeline_status(tenant_id, timeline_id)
        .await?
        .json()
        .await
        .context("decode safekeeper timeline status")?;
    Ok(status.flush_lsn)
}

/// The pageserver CLI tenant operations target: the first-configured
/// node, which serves the tenant's main timeline. Branch timelines get
/// their own pageservers, resolved via `pageserver_conf_for_timeline`.
fn get_default_pageserver(env: &local_env::LocalEnv) -> anyhow::Result<PageServerNode> {
    let ps_conf = env.pageservers.first().context(
        "no pageserver provisioned yet — create an endpoint (`endpoint create`) first",
    )?;
    Ok(PageServerNode::from_env(env, ps_conf))
}

async fn handle_tenant(subcmd: &TenantCmd, env: &mut local_env::LocalEnv) -> anyhow::Result<()> {
    match subcmd {
        TenantCmd::List => {
            for ps_conf in &env.pageservers {
                let ps = PageServerNode::from_env(env, ps_conf);
                match ps.read_timeline_identity()? {
                    Some((tenant_id, timeline_id)) => {
                        println!("{tenant_id} (timeline {timeline_id}, pageserver {})", ps_conf.id)
                    }
                    None => println!("(pageserver {}: no tenant assigned)", ps_conf.id),
                }
            }
        }
        TenantCmd::Import(args) => {
            let tenant_id = args.tenant_id;

            let storage_controller = StorageController::from_env(env);
            let create_response = storage_controller.tenant_import(tenant_id).await?;

            let shard_zero = create_response
                .shards
                .first()
                .expect("Import response omitted shards");

            let attached_pageserver_id = shard_zero.node_id;
            let pageserver =
                PageServerNode::from_env(env, env.get_pageserver_conf(attached_pageserver_id)?);

            println!(
                "Imported tenant {tenant_id}, attached to pageserver {attached_pageserver_id}"
            );

            let timelines = pageserver
                .http_client
                .list_timelines(shard_zero.shard_id)
                .await?;

            // Pick a 'main' timeline that has no ancestors, the rest will get arbitrary names
            let main_timeline = timelines
                .iter()
                .find(|t| t.ancestor_timeline_id.is_none())
                .expect("No timelines found")
                .timeline_id;

            let mut branch_i = 0;
            for timeline in timelines.iter() {
                let branch_name = if timeline.timeline_id == main_timeline {
                    "main".to_string()
                } else {
                    branch_i += 1;
                    format!("branch_{branch_i}")
                };

                println!(
                    "Importing timeline {tenant_id}/{} as branch {branch_name}",
                    timeline.timeline_id
                );

                env.register_branch_mapping(branch_name, tenant_id, timeline.timeline_id)
                    .await?;
            }
        }
        TenantCmd::Create(args) => {
            // Effective tenant config is the [tenant_config] section of
            // pageserver.toml; there is no per-tenant override store.
            anyhow::ensure!(
                args.config.is_empty(),
                "per-tenant config overrides are not supported: set [tenant_config] in \
                 pageserver.toml instead"
            );

            // If tenant ID was not specified, generate one
            let tenant_id = args.tenant_id.unwrap_or_else(TenantId::generate);
            let new_timeline_id = args.timeline_id.unwrap_or(TimelineId::generate());

            // Co-located topology: the tenant's PS+SK pair is provisioned
            // under the first endpoint created for this timeline, not here.
            // `tenant create` just allocates the ids and records the pending
            // Bootstrap recipe in the storage controller (durable cluster
            // state in YB) so `endpoint create` knows to run initdb + import.
            // The `main` branch mapping is registered with the controller
            // only after the timeline exists (during provisioning) — the
            // controller rejects a branch name for a timeline it doesn't
            // know yet, so until then the pending row carries the name.
            let sc = StorageController::from_env(env);
            sc.record_pending_timeline_init(
                tenant_id,
                new_timeline_id,
                pageserver_api::controller_api::PendingTimelineInitRequest {
                    branch_name: DEFAULT_BRANCH_NAME.to_string(),
                    pg_version: args.pg_version,
                    ancestor_timeline_id: None,
                    start_lsn: Lsn(0),
                },
            )
            .await?;

            // NB: test/e2e/run.sh extracts the tenant id from this exact
            // line (`awk '/successfully created/{print $2}'`). The
            // pageserver that serves it comes up at `endpoint create`.
            println!("tenant {tenant_id} successfully created on the pageserver");
            println!("Created an initial timeline '{new_timeline_id}' for tenant: {tenant_id}",);

            if args.set_default {
                println!("Setting tenant {tenant_id} as a default one");
                env.default_tenant_id = Some(tenant_id);
            }
        }
        TenantCmd::SetDefault(args) => {
            println!("Setting tenant {} as a default one", args.tenant_id);
            env.default_tenant_id = Some(args.tenant_id);
        }
        TenantCmd::Config(_) => {
            anyhow::bail!(
                "per-tenant config overrides are not supported: the effective config is the \
                 [tenant_config] section of the pageserver's pageserver.toml"
            );
        }
    }
    Ok(())
}

async fn handle_timeline(cmd: &TimelineCmd, env: &mut local_env::LocalEnv) -> Result<()> {
    match cmd {
        TimelineCmd::List(args) => {
            // TODO(sharding): this command shouldn't have to specify a shard ID: we should ask the storage controller
            // where shard 0 is attached, and query there.
            let pageserver = get_default_pageserver(env)?;
            let tenant_shard_id = get_tenant_shard_id(args.tenant_shard_id, env)?;
            let timelines = pageserver.timeline_list(&tenant_shard_id).await?;
            print_timelines_tree(timelines, env.timeline_name_mappings().await?)?;
        }
        TimelineCmd::Create(_) => {
            anyhow::bail!(
                "`timeline create` is not supported: each pageserver serves exactly one \
                 timeline (created by `tenant create`). A second timeline requires a second \
                 pageserver — per-endpoint pageserver support is the next step."
            );
        }
        TimelineCmd::Import(_) => {
            anyhow::bail!(
                "`timeline import` is not supported: the pageserver no longer exposes the \
                 basebackup/WAL import API. Bootstrap a fresh timeline with `tenant create`."
            );
        }
        TimelineCmd::Branch(args) => {
            let tenant_id = get_tenant_id(args.tenant_id, env)?;
            let new_timeline_id = args.timeline_id.unwrap_or(TimelineId::generate());
            let new_branch_name = &args.branch_name;
            let ancestor_branch_name = args
                .ancestor_branch_name
                .clone()
                .unwrap_or(DEFAULT_BRANCH_NAME.to_owned());
            let ancestor_timeline_id = env
                .get_branch_timeline_id(&ancestor_branch_name, tenant_id)
                .await?
                .ok_or_else(|| {
                    anyhow!("Found no timeline id for branch name '{ancestor_branch_name}'")
                })?;

            // Co-located topology: a branch is a NEW pageserver +
            // safekeeper pair, but they aren't provisioned here — that
            // happens under the first endpoint created for the branch
            // (`endpoint create`). Here we only capture the branch point
            // and pg_version from the ancestor (which is running now) and
            // record a pending Branch init. The branch's pre-branch data
            // stays under the ancestor's timeline_id in YB and is served
            // via lineage-aware reads — nothing is copied.
            let ancestor_ps_conf = env
                .pageserver_conf_for_timeline(ancestor_timeline_id)
                .with_context(|| {
                    format!("no pageserver serves ancestor timeline {ancestor_timeline_id}")
                })?
                .clone();
            let ancestor_ps = PageServerNode::from_env(env, &ancestor_ps_conf);
            let ancestor_info = ancestor_ps
                .timeline_info(
                    TenantId::unsharded(tenant_id),
                    ancestor_timeline_id,
                    pageserver_client::mgmt_api::ForceAwaitLogicalSize::No,
                )
                .await
                .context("fetch ancestor timeline info")?;
            let pg_version = ancestor_info.pg_version;

            // Default branch point: the ancestor's durable WAL end, from
            // its safekeeper. The pageserver's last_record_lsn is its
            // ingest position, which can lag the compute's flushed WAL
            // by an arbitrary amount — branching there would cut a
            // recently-committed transaction out of the branch.
            let start_lsn = match args.ancestor_start_lsn {
                Some(lsn) => lsn,
                None => {
                    ancestor_durable_lsn(env, tenant_id, ancestor_timeline_id)
                        .await
                        .context("fetch ancestor durable WAL end from safekeeper")?
                }
            };

            // The branch-name mapping is registered with the storage
            // controller only after the timeline exists (during
            // provisioning); until then the pending row carries the branch
            // name so `endpoint create` can resolve it.
            let sc = StorageController::from_env(env);
            sc.record_pending_timeline_init(
                tenant_id,
                new_timeline_id,
                pageserver_api::controller_api::PendingTimelineInitRequest {
                    branch_name: new_branch_name.to_string(),
                    pg_version,
                    ancestor_timeline_id: Some(ancestor_timeline_id),
                    start_lsn,
                },
            )
            .await?;

            println!(
                "Created branch '{new_branch_name}' (timeline {new_timeline_id}) at {start_lsn} \
                 from '{ancestor_branch_name}'; its pageserver + safekeeper come up at \
                 `endpoint create`",
            );
        }
    }

    Ok(())
}

/// Provision the pageserver + safekeeper pair that serves `timeline_id`,
/// co-located under `endpoint_id`, consuming the pending init recorded at
/// `tenant create` / `timeline branch`. No-op when a pageserver already
/// serves the timeline (a second endpoint on an already-served branch —
/// the pair stays under the first endpoint that created it).
async fn provision_timeline_for_endpoint(
    env: &mut LocalEnv,
    endpoint_id: &str,
    tenant_id: TenantId,
    timeline_id: TimelineId,
) -> anyhow::Result<()> {
    if env.pageserver_conf_for_timeline(timeline_id).is_some() {
        return Ok(());
    }
    // Read the recipe from the storage controller (durable cluster state in
    // YB). Scoped so the borrow of `env` ends before the `&mut env`
    // provisioning calls below.
    let pending = {
        let sc = StorageController::from_env(env);
        sc.list_pending_timeline_inits(tenant_id)
            .await?
            .into_iter()
            .find(|p| p.timeline_id == timeline_id)
            .with_context(|| {
                format!(
                    "timeline {timeline_id} has no provisioned pageserver and no pending init; \
                     run `tenant create` (for the main branch) or `timeline branch` first"
                )
            })?
    };

    match pending.ancestor_timeline_id {
        // No ancestor => Bootstrap (the tenant's initial timeline).
        None => {
            provision_bootstrap(env, endpoint_id, tenant_id, timeline_id, pending.pg_version)
                .await?;
        }
        Some(ancestor_timeline_id) => {
            provision_branch(
                env,
                endpoint_id,
                tenant_id,
                timeline_id,
                ancestor_timeline_id,
                pending.start_lsn,
                pending.pg_version,
            )
            .await?;
        }
    }

    // The timeline now exists in the storage controller, so it will accept
    // the branch-name mapping (deferred from `tenant create` / `timeline
    // branch`).
    env.register_branch_mapping(pending.branch_name, tenant_id, timeline_id)
        .await?;

    // The pair is materialized; drop the pending recipe from YB.
    StorageController::from_env(env)
        .delete_pending_timeline_init(tenant_id, timeline_id)
        .await?;
    Ok(())
}

/// Bring up the main timeline's pair: start the safekeeper, then start the
/// pageserver (which runs initdb + import to bootstrap the timeline), then
/// have the storage controller create the timeline on the safekeeper.
async fn provision_bootstrap(
    env: &mut LocalEnv,
    endpoint_id: &str,
    tenant_id: TenantId,
    timeline_id: TimelineId,
    pg_version: PgMajorVersion,
) -> anyhow::Result<()> {
    let ps_conf = allocate_pageserver_conf(env, endpoint_id)?;
    let sk_conf = allocate_safekeeper_conf(env, endpoint_id, timeline_id)?;
    // Record the safekeeper before building the pageserver toml: its
    // `safekeeper_pg_addr` is taken from `env.safekeepers.first()`.
    env.safekeepers.push(sk_conf.clone());

    // Safekeeper first: it must be up (and registered with the storage
    // controller) before the controller creates the timeline on it.
    println!(
        "Provisioning safekeeper {} (pg port {}) for endpoint '{endpoint_id}'",
        sk_conf.id, sk_conf.pg_port
    );
    std::fs::create_dir_all(env.safekeeper_data_dir(endpoint_id))?;
    let safekeeper = SafekeeperNode::from_env(env, &sk_conf);
    safekeeper.initialize()?;
    safekeeper
        .start(&[], &std::time::Duration::from_secs(30))
        .await
        .context("start main safekeeper")?;
    let storage_controller = StorageController::from_env(env);
    storage_controller
        .register_safekeeper(&sk_conf)
        .await
        .context("register main safekeeper with storage controller")?;

    // Pageserver: bake the Bootstrap identity, start it (runs initdb +
    // import). Mirrors the body of the old `tenant create`.
    println!(
        "Provisioning pageserver {} (pg {}) for endpoint '{endpoint_id}'",
        ps_conf.id, ps_conf.listen_pg_addr
    );
    std::fs::create_dir_all(&ps_conf.data_dir)?;
    let init_conf = pageserver_init_conf_for(env, &ps_conf)?;
    let pageserver = PageServerNode::from_env(env, &ps_conf);
    pageserver
        .initialize(init_conf, &format!("127.0.0.1:{}", sk_conf.pg_port))
        .context("initialize main pageserver")?;
    pageserver.set_timeline_identity(
        tenant_id,
        timeline_id,
        0, // placeholder; start() bumps via attach_hook
        Some(&pageserver_api::config::TimelineInitConfig::Bootstrap { pg_version }),
    )?;
    pageserver
        .register_and_attach()
        .await
        .context("register tenant with storage controller")?;
    pageserver
        .start(&std::time::Duration::from_secs(60))
        .await
        .context("start main pageserver")?;

    // Bootstrap's start LSN isn't known until the pageserver has run
    // initdb; wait for the timeline to come up Active and read it back.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(120);
    let start_lsn = loop {
        match pageserver
            .timeline_info(
                TenantId::unsharded(tenant_id),
                timeline_id,
                pageserver_client::mgmt_api::ForceAwaitLogicalSize::No,
            )
            .await
        {
            Ok(info) if matches!(info.state, pageserver_api::models::TimelineState::Active) => {
                break info.last_record_lsn;
            }
            Ok(_) | Err(_) if std::time::Instant::now() < deadline => {
                tokio::time::sleep(std::time::Duration::from_millis(500)).await;
            }
            Ok(info) => {
                anyhow::bail!(
                    "bootstrap timeline did not become Active in time (state: {:?})",
                    info.state
                );
            }
            Err(e) => {
                return Err(e.context("bootstrap timeline did not become Active in time"));
            }
        }
    };

    // Create the timeline on its co-located safekeeper with an explicit
    // single-member configuration, pinning placement to this pair. The
    // storage controller's global safekeeper scheduler would otherwise
    // pick any registered safekeeper — wrong in the co-located topology
    // once more than one pair exists (the compute talks to its own
    // safekeeper). Mirrors `provision_branch`.
    let sk_host = sk_conf
        .listen_addr
        .clone()
        .unwrap_or_else(|| "localhost".to_string());
    let mconf = safekeeper_api::membership::Configuration {
        generation: SafekeeperGeneration::new(1),
        members: safekeeper_api::membership::MemberSet {
            m: vec![SafekeeperId {
                host: sk_host,
                id: sk_conf.id,
                pg_port: sk_conf.get_compute_port(),
            }],
        },
        new_members: None,
    };
    safekeeper
        .create_timeline(&safekeeper_api::models::TimelineCreateRequest {
            tenant_id,
            timeline_id,
            mconf,
            pg_version: PgVersionId::from(pg_version),
            system_id: None,
            wal_seg_size: None,
            start_lsn,
            commit_lsn: None,
        })
        .await
        .context("create bootstrap timeline on safekeeper")?;

    // Record the launcher-created timeline in the storage controller with
    // an explicit sk_set pinning the co-located safekeeper (so the
    // pageserver streams from it too, and the row reflects real placement).
    storage_controller
        .timeline_register(
            tenant_id,
            timeline_id,
            pageserver_api::controller_api::TimelineRegisterRequest {
                start_lsn,
                sk_set: vec![sk_conf.id],
            },
        )
        .await
        .context("register bootstrap timeline with storage controller")?;

    env.pageservers.push(ps_conf);
    Ok(())
}

/// Bring up a branch's pair: create the safekeeper at the branch LSN, then
/// start the pageserver cloned from the ancestor's config with a Branch
/// identity, then register the branch timeline with the storage controller.
#[allow(clippy::too_many_arguments)]
async fn provision_branch(
    env: &mut LocalEnv,
    endpoint_id: &str,
    tenant_id: TenantId,
    timeline_id: TimelineId,
    ancestor_timeline_id: TimelineId,
    start_lsn: Lsn,
    pg_version: PgMajorVersion,
) -> anyhow::Result<()> {
    let ancestor_ps_conf = env
        .pageserver_conf_for_timeline(ancestor_timeline_id)
        .with_context(|| {
            format!("no pageserver serves ancestor timeline {ancestor_timeline_id}")
        })?
        .clone();
    let ancestor_ps = PageServerNode::from_env(env, &ancestor_ps_conf);

    let ps_conf = allocate_pageserver_conf(env, endpoint_id)?;
    let sk_conf = allocate_safekeeper_conf(env, endpoint_id, timeline_id)?;
    env.safekeepers.push(sk_conf.clone());

    // Safekeeper first: it must hold the timeline (starting at the branch
    // LSN) before the pageserver's walreceiver and the compute's
    // walproposer dial in.
    println!(
        "Provisioning safekeeper {} (pg port {}) for endpoint '{endpoint_id}'",
        sk_conf.id, sk_conf.pg_port
    );
    std::fs::create_dir_all(env.safekeeper_data_dir(endpoint_id))?;
    let safekeeper = SafekeeperNode::from_env(env, &sk_conf);
    safekeeper.initialize()?;
    safekeeper
        .start(&[], &std::time::Duration::from_secs(30))
        .await
        .context("start branch safekeeper")?;
    let sk_host = sk_conf
        .listen_addr
        .clone()
        .unwrap_or_else(|| "localhost".to_string());
    let mconf = safekeeper_api::membership::Configuration {
        generation: SafekeeperGeneration::new(1),
        members: safekeeper_api::membership::MemberSet {
            m: vec![SafekeeperId {
                host: sk_host,
                id: sk_conf.id,
                pg_port: sk_conf.get_compute_port(),
            }],
        },
        new_members: None,
    };
    safekeeper
        .create_timeline(&safekeeper_api::models::TimelineCreateRequest {
            tenant_id,
            timeline_id,
            mconf,
            pg_version: PgVersionId::from(pg_version),
            system_id: None,
            wal_seg_size: None,
            start_lsn,
            commit_lsn: None,
        })
        .await
        .context("create branch timeline on safekeeper")?;

    // Pageserver: clone the ancestor's config, bake the branch identity,
    // start. The process writes the branch's timeline_state row at startup
    // ([timeline_init] branch) and builds its read lineage from the
    // ancestor links.
    println!(
        "Provisioning pageserver {} (pg {}) for endpoint '{endpoint_id}'",
        ps_conf.id, ps_conf.listen_pg_addr
    );
    std::fs::create_dir_all(&ps_conf.data_dir)?;
    let pageserver = PageServerNode::from_env(env, &ps_conf);
    pageserver.initialize_from_template(&ancestor_ps, &format!("127.0.0.1:{}", sk_conf.pg_port))?;
    pageserver.set_timeline_identity(
        tenant_id,
        timeline_id,
        0, // placeholder; start() bumps via attach_hook
        Some(&pageserver_api::config::TimelineInitConfig::Branch {
            ancestor_timeline_id,
            ancestor_lsn: start_lsn,
        }),
    )?;
    pageserver
        .start(&std::time::Duration::from_secs(60))
        .await
        .context("start branch pageserver")?;

    // Wait for the branch timeline to come up Active.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(120);
    loop {
        match pageserver
            .timeline_info(
                TenantId::unsharded(tenant_id),
                timeline_id,
                pageserver_client::mgmt_api::ForceAwaitLogicalSize::No,
            )
            .await
        {
            Ok(info) if matches!(info.state, pageserver_api::models::TimelineState::Active) => {
                break;
            }
            Ok(_) | Err(_) if std::time::Instant::now() < deadline => {
                tokio::time::sleep(std::time::Duration::from_millis(500)).await;
            }
            Ok(info) => {
                anyhow::bail!(
                    "branch timeline did not become Active in time (state: {:?})",
                    info.state
                );
            }
            Err(e) => {
                return Err(e.context("branch timeline did not become Active in time"));
            }
        }
    }

    // Record the launcher-created timeline in the storage controller's
    // bookkeeping (branch names + deletion flows hang off the timelines row).
    let storage_controller = StorageController::from_env(env);
    storage_controller
        .timeline_register(
            tenant_id,
            timeline_id,
            pageserver_api::controller_api::TimelineRegisterRequest {
                start_lsn,
                sk_set: vec![sk_conf.id],
            },
        )
        .await
        .context("register branch timeline with storage controller")?;

    env.pageservers.push(ps_conf);
    Ok(())
}

async fn handle_endpoint(subcmd: &EndpointCmd, env: &mut local_env::LocalEnv) -> Result<()> {
    let mut cplane = ComputeControlPlane::load(env.clone()).await?;

    match subcmd {
        EndpointCmd::List(args) => {
            // TODO(sharding): this command shouldn't have to specify a shard ID: we should ask the storage controller
            // where shard 0 is attached, and query there.
            let tenant_shard_id = get_tenant_shard_id(args.tenant_shard_id, env)?;

            let timeline_name_mappings = env.timeline_name_mappings().await?;

            let mut table = comfy_table::Table::new();

            table.load_preset(comfy_table::presets::NOTHING);

            table.set_header([
                "ENDPOINT",
                "ADDRESS",
                "TIMELINE",
                "BRANCH NAME",
                "LSN",
                "STATUS",
            ]);

            for (endpoint_id, endpoint) in cplane
                .endpoints
                .iter()
                .filter(|(_, endpoint)| endpoint.tenant_id == tenant_shard_id)
            {
                let lsn_str = match endpoint.mode {
                    ComputeMode::Static(lsn) => {
                        // -> read-only endpoint
                        // Use the node's LSN.
                        lsn.to_string()
                    }
                    _ => {
                        // As the LSN here refers to the one that the compute is started with,
                        // we display nothing as it is a primary/hot standby compute.
                        "---".to_string()
                    }
                };

                let branch_name = timeline_name_mappings
                    .get(&TenantTimelineId::new(
                        tenant_shard_id,
                        endpoint.timeline_id,
                    ))
                    .map(|name| name.as_str())
                    .unwrap_or("?");

                table.add_row([
                    endpoint_id.as_str(),
                    &endpoint.pg_address.to_string(),
                    &endpoint.timeline_id.to_string(),
                    branch_name,
                    lsn_str.as_str(),
                    &format!("{}", endpoint.status()),
                ]);
            }

            println!("{table}");
        }
        EndpointCmd::Create(args) => {
            let tenant_id = get_tenant_id(args.tenant_id, env)?;
            let branch_name = args
                .branch_name
                .clone()
                .unwrap_or(DEFAULT_BRANCH_NAME.to_owned());
            let endpoint_id = args
                .endpoint_id
                .clone()
                .unwrap_or_else(|| format!("ep-{branch_name}"));

            // Resolve the branch to its timeline. A provisioned branch is
            // known to the storage controller via its materialized branch
            // name; an as-yet-unprovisioned one (recorded by `tenant create`
            // / `timeline branch`, mapping not registered yet) is found in
            // the controller's pending-init rows.
            let timeline_id = match env.get_branch_timeline_id(&branch_name, tenant_id).await? {
                Some(tid) => tid,
                None => StorageController::from_env(env)
                    .list_pending_timeline_inits(tenant_id)
                    .await?
                    .into_iter()
                    .find(|p| p.branch_name == branch_name)
                    .map(|p| p.timeline_id)
                    .ok_or_else(|| {
                        anyhow!("Found no timeline id for branch name '{branch_name}'")
                    })?,
            };

            let mode = match (args.lsn, args.hot_standby) {
                (Some(lsn), false) => ComputeMode::Static(lsn),
                (None, true) => ComputeMode::Replica,
                (None, false) => ComputeMode::Primary,
                (Some(_), true) => anyhow::bail!("cannot specify both lsn and hot-standby"),
            };

            match (mode, args.hot_standby) {
                (ComputeMode::Static(_), true) => {
                    bail!(
                        "Cannot start a node in hot standby mode when it is already configured as a static replica"
                    )
                }
                (ComputeMode::Primary, true) => {
                    bail!(
                        "Cannot start a node as a hot standby replica, it is already configured as primary node"
                    )
                }
                _ => {}
            }

            if !args.allow_multiple {
                cplane.check_conflicting_endpoints(mode, tenant_id, timeline_id)?;
            }

            // Provision the timeline's pageserver + safekeeper under this
            // endpoint's folder if nothing serves it yet (running the
            // pending Bootstrap/Branch init from `tenant create` /
            // `timeline branch`). A second endpoint on an already-served
            // branch skips this — the pair stays under the first endpoint.
            provision_timeline_for_endpoint(env, &endpoint_id, tenant_id, timeline_id).await?;

            // One endpoint_storage instance per endpoint, under this
            // endpoint's folder. Unlike the PS/SK pair (one per timeline,
            // shared by siblings), every endpoint gets its own.
            provision_endpoint_storage(env, &endpoint_id).await?;

            cplane.new_endpoint(
                &endpoint_id,
                tenant_id,
                timeline_id,
                args.pg_port,
                args.external_http_port,
                args.internal_http_port,
                args.pg_version,
                mode,
                args.grpc,
                !args.update_catalog,
                false,
                args.privileged_role_name.clone(),
                args.shared_buffers.clone(),
                args.lfc_size.clone(),
                args.pg_settings.clone(),
            )
            .await?;
        }
        EndpointCmd::Start(args) => {
            let endpoint_id = &args.endpoint_id;
            let pageserver_id = args.endpoint_pageserver_id;
            let remote_ext_base_url = &args.remote_ext_base_url;

            let default_generation = env
                .storage_controller
                .timelines_onto_safekeepers
                .then_some(1);
            let safekeepers_generation = args
                .safekeepers_generation
                .or(default_generation)
                .map(SafekeeperGeneration::new);
            let endpoint = cplane
                .endpoints
                .get(endpoint_id.as_str())
                .ok_or_else(|| anyhow!("endpoint {endpoint_id} not found"))?;

            // If --safekeepers argument is given, use only the listed
            // safekeeper nodes; otherwise the safekeepers serving this
            // endpoint's timeline (its dedicated one for branches, the
            // legacy set for the main timeline).
            let safekeepers = if let Some(safekeepers) = parse_safekeepers(&args.safekeepers)? {
                safekeepers
            } else {
                env.safekeepers_for_timeline(endpoint.timeline_id)
                    .iter()
                    .map(|sk| sk.id)
                    .collect()
            };

            if !args.allow_multiple {
                cplane.check_conflicting_endpoints(
                    endpoint.mode,
                    endpoint.tenant_id,
                    endpoint.timeline_id,
                )?;
            }

            let prefer_protocol = if endpoint.grpc {
                PageserverProtocol::Grpc
            } else {
                PageserverProtocol::Libpq
            };

            let mut pageserver_conninfo = if let Some(ps_id) = pageserver_id {
                let conf = env.get_pageserver_conf(ps_id).unwrap();
                local_pageserver_conf_to_conn_info(conf, env.use_local_tls)?
            } else {
                // Per-endpoint pageserver topology: the pageserver
                // serving this endpoint's timeline is identified by
                // the identity in its pageserver.toml.
                let conf = env
                    .pageserver_conf_for_timeline(endpoint.timeline_id)
                    .with_context(|| {
                        format!("no pageserver serves timeline {}", endpoint.timeline_id)
                    })?;

                // Initialize an LSN lease for static computes.
                if let ComputeMode::Static(lsn) = endpoint.mode {
                    let pageserver = PageServerNode::from_env(env, conf);
                    pageserver
                        .http_client
                        .timeline_init_lsn_lease(
                            TenantId::unsharded(endpoint.tenant_id),
                            endpoint.timeline_id,
                            lsn,
                        )
                        .await?;
                }

                local_pageserver_conf_to_conn_info(conf, env.use_local_tls)?
            };
            pageserver_conninfo.prefer_protocol = prefer_protocol;

            let ps_conf = env.get_pageserver_conf(DEFAULT_PAGESERVER_ID)?;
            let auth_token = if matches!(ps_conf.pg_auth_type, AuthType::YbLightJWT) {
                let claims = Claims::new(Some(endpoint.tenant_id), Scope::Tenant);

                Some(env.generate_auth_token(&claims)?)
            } else {
                None
            };

            let exp = (std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH)?
                + Duration::from_secs(86400))
            .as_secs();
            let claims = endpoint_storage::claims::EndpointStorageClaims {
                tenant_id: endpoint.tenant_id,
                timeline_id: endpoint.timeline_id,
                endpoint_id: endpoint_id.to_string(),
                exp,
            };

            let endpoint_storage_token = env.generate_auth_token(&claims)?;
            // Point the compute at this endpoint's own endpoint_storage
            // instance. Fall back to the template addr only if none was
            // provisioned (e.g. an old binary skipped it in compat tests).
            let endpoint_storage_addr = env
                .endpoint_storages
                .iter()
                .find(|e| e.endpoint_id == **endpoint_id)
                .map(|e| e.listen_addr)
                .unwrap_or(env.endpoint_storage.listen_addr)
                .to_string();

            let args = control_plane::endpoint::EndpointStartArgs {
                auth_token,
                endpoint_storage_token,
                endpoint_storage_addr,
                safekeepers_generation,
                safekeepers,
                pageserver_conninfo,
                remote_ext_base_url: remote_ext_base_url.clone(),
                create_test_user: args.create_test_user,
                start_timeout: args.start_timeout,
                autoprewarm: args.autoprewarm,
                offload_lfc_interval_seconds: args.offload_lfc_interval_seconds,
                dev: args.dev,
            };

            println!("Starting existing endpoint {endpoint_id}...");
            endpoint.start(args).await?;
        }
        EndpointCmd::UpdatePageservers(args) => {
            let endpoint_id = &args.endpoint_id;
            let endpoint = cplane
                .endpoints
                .get(endpoint_id.as_str())
                .with_context(|| format!("postgres endpoint {endpoint_id} is not found"))?;
            let prefer_protocol = if endpoint.grpc {
                PageserverProtocol::Grpc
            } else {
                PageserverProtocol::Libpq
            };
            let mut pageserver_conninfo = match args.pageserver_id {
                Some(pageserver_id) => {
                    let conf = env.get_pageserver_conf(pageserver_id)?;
                    local_pageserver_conf_to_conn_info(conf, env.use_local_tls)?
                }
                None => {
                    let conf = env
                        .pageserver_conf_for_timeline(endpoint.timeline_id)
                        .with_context(|| {
                            format!("no pageserver serves timeline {}", endpoint.timeline_id)
                        })?;
                    local_pageserver_conf_to_conn_info(conf, env.use_local_tls)?
                }
            };
            pageserver_conninfo.prefer_protocol = prefer_protocol;

            endpoint
                .update_pageservers_in_config(&pageserver_conninfo)
                .await?;
        }
        EndpointCmd::Reconfigure(args) => {
            let endpoint_id = &args.endpoint_id;
            let endpoint = cplane
                .endpoints
                .get(endpoint_id.as_str())
                .with_context(|| format!("postgres endpoint {endpoint_id} is not found"))?;

            let prefer_protocol = if endpoint.grpc {
                PageserverProtocol::Grpc
            } else {
                PageserverProtocol::Libpq
            };
            let mut pageserver_conninfo = if let Some(ps_id) = args.endpoint_pageserver_id {
                let conf = env.get_pageserver_conf(ps_id)?;
                local_pageserver_conf_to_conn_info(conf, env.use_local_tls)?
            } else {
                let conf = env
                    .pageserver_conf_for_timeline(endpoint.timeline_id)
                    .with_context(|| {
                        format!("no pageserver serves timeline {}", endpoint.timeline_id)
                    })?;
                local_pageserver_conf_to_conn_info(conf, env.use_local_tls)?
            };
            pageserver_conninfo.prefer_protocol = prefer_protocol;

            // If --safekeepers argument is given, use only the listed
            // safekeeper nodes; otherwise all from the env.
            let safekeepers = parse_safekeepers(&args.safekeepers)?;
            endpoint
                .reconfigure(Some(&pageserver_conninfo), safekeepers, None)
                .await?;
        }
        EndpointCmd::RefreshConfiguration(args) => {
            let endpoint_id = &args.endpoint_id;
            let endpoint = cplane
                .endpoints
                .get(endpoint_id.as_str())
                .with_context(|| format!("postgres endpoint {endpoint_id} is not found"))?;
            endpoint.refresh_configuration().await?;
        }
        EndpointCmd::Stop(args) => {
            let endpoint_id = &args.endpoint_id;
            let endpoint = cplane
                .endpoints
                .get(endpoint_id)
                .with_context(|| format!("postgres endpoint {endpoint_id} is not found"))?;
            match endpoint.stop(args.mode, args.destroy).await?.lsn {
                Some(lsn) => println!("{lsn}"),
                None => println!("null"),
            }
        }
        EndpointCmd::GenerateJwt(args) => {
            let endpoint = {
                let endpoint_id = &args.endpoint_id;

                cplane
                    .endpoints
                    .get(endpoint_id)
                    .with_context(|| format!("postgres endpoint {endpoint_id} is not found"))?
            };

            let jwt = endpoint.generate_jwt(args.scope)?;

            print!("{jwt}");
        }
    }

    Ok(())
}

/// Parse --safekeepers as list of safekeeper ids.
fn parse_safekeepers(safekeepers_str: &Option<String>) -> Result<Option<Vec<NodeId>>> {
    if let Some(safekeepers_str) = safekeepers_str {
        let mut safekeepers: Vec<NodeId> = Vec::new();
        for sk_id in safekeepers_str.split(',').map(str::trim) {
            let sk_id = NodeId(
                u64::from_str(sk_id)
                    .map_err(|_| anyhow!("invalid node ID \"{sk_id}\" in --safekeepers list"))?,
            );
            safekeepers.push(sk_id);
        }
        Ok(Some(safekeepers))
    } else {
        Ok(None)
    }
}

async fn handle_mappings(subcmd: &MappingsCmd, env: &local_env::LocalEnv) -> Result<()> {
    match subcmd {
        MappingsCmd::Map(args) => {
            env.register_branch_mapping(
                args.branch_name.to_owned(),
                args.tenant_id,
                args.timeline_id,
            )
            .await?;

            Ok(())
        }
    }
}

fn get_pageserver(
    env: &local_env::LocalEnv,
    pageserver_id_arg: Option<NodeId>,
) -> Result<PageServerNode> {
    let node_id = pageserver_id_arg.unwrap_or(DEFAULT_PAGESERVER_ID);

    Ok(PageServerNode::from_env(
        env,
        env.get_pageserver_conf(node_id)?,
    ))
}

async fn handle_pageserver(subcmd: &PageserverCmd, env: &local_env::LocalEnv) -> Result<()> {
    match subcmd {
        PageserverCmd::Start(args) => {
            if let Err(e) = get_pageserver(env, args.pageserver_id)?
                .start(&args.start_timeout)
                .await
            {
                eprintln!("pageserver start failed: {e}");
                exit(1);
            }
        }

        PageserverCmd::Stop(args) => {
            let immediate = match args.stop_mode {
                StopMode::Fast => false,
                StopMode::Immediate => true,
            };
            if let Err(e) = get_pageserver(env, args.pageserver_id)?.stop(immediate) {
                eprintln!("pageserver stop failed: {e}");
                exit(1);
            }
        }

        PageserverCmd::Restart(args) => {
            let pageserver = get_pageserver(env, args.pageserver_id)?;
            //TODO what shutdown strategy should we use here?
            if let Err(e) = pageserver.stop(false) {
                eprintln!("pageserver stop failed: {e}");
                exit(1);
            }

            if let Err(e) = pageserver.start(&args.start_timeout).await {
                eprintln!("pageserver start failed: {e}");
                exit(1);
            }
        }

        PageserverCmd::Status(args) => {
            match get_pageserver(env, args.pageserver_id)?
                .check_status()
                .await
            {
                Ok(_) => println!("Page server is up and running"),
                Err(err) => {
                    eprintln!("Page server is not available: {err}");
                    exit(1);
                }
            }
        }
    }
    Ok(())
}

async fn handle_storage_controller(
    subcmd: &StorageControllerCmd,
    env: &local_env::LocalEnv,
) -> Result<()> {
    let svc = StorageController::from_env(env);
    match subcmd {
        StorageControllerCmd::Start(args) => {
            let start_args = YbLightStorageControllerStartArgs {
                instance_id: args.instance_id,
                base_port: args.base_port,
                start_timeout: args.start_timeout,
            };

            if let Err(e) = svc.start(start_args).await {
                eprintln!("start failed: {e}");
                exit(1);
            }
        }

        StorageControllerCmd::Stop(args) => {
            let stop_args = YbLightStorageControllerStopArgs {
                instance_id: args.instance_id,
                immediate: match args.stop_mode {
                    StopMode::Fast => false,
                    StopMode::Immediate => true,
                },
            };
            if let Err(e) = svc.stop(stop_args).await {
                eprintln!("stop failed: {e}");
                exit(1);
            }
        }
    }
    Ok(())
}

fn get_safekeeper(env: &local_env::LocalEnv, id: NodeId) -> Result<SafekeeperNode> {
    if let Some(node) = env.safekeepers.iter().find(|node| node.id == id) {
        Ok(SafekeeperNode::from_env(env, node))
    } else {
        bail!("could not find safekeeper {id}")
    }
}

async fn handle_safekeeper(subcmd: &SafekeeperCmd, env: &local_env::LocalEnv) -> Result<()> {
    match subcmd {
        SafekeeperCmd::Start(args) => {
            let safekeeper = get_safekeeper(env, args.id)?;

            if let Err(e) = safekeeper.start(&args.extra_opt, &args.start_timeout).await {
                eprintln!("safekeeper start failed: {e}");
                exit(1);
            }
        }

        SafekeeperCmd::Stop(args) => {
            let safekeeper = get_safekeeper(env, args.id)?;
            let immediate = match args.stop_mode {
                StopMode::Fast => false,
                StopMode::Immediate => true,
            };
            if let Err(e) = safekeeper.stop(immediate) {
                eprintln!("safekeeper stop failed: {e}");
                exit(1);
            }
        }

        SafekeeperCmd::Restart(args) => {
            let safekeeper = get_safekeeper(env, args.id)?;
            let immediate = match args.stop_mode {
                StopMode::Fast => false,
                StopMode::Immediate => true,
            };

            if let Err(e) = safekeeper.stop(immediate) {
                eprintln!("safekeeper stop failed: {e}");
                exit(1);
            }

            if let Err(e) = safekeeper.start(&args.extra_opt, &args.start_timeout).await {
                eprintln!("safekeeper start failed: {e}");
                exit(1);
            }
        }
    }
    Ok(())
}

async fn handle_endpoint_storage(
    subcmd: &EndpointStorageCmd,
    env: &local_env::LocalEnv,
) -> Result<()> {
    use EndpointStorageCmd::*;

    // Per-endpoint topology: start/stop every provisioned instance.
    match subcmd {
        Start(EndpointStorageStartCmd { start_timeout }) => {
            for es_conf in &env.endpoint_storages {
                let storage = EndpointStorage::from_conf(env, es_conf);
                // In tests like test_forward_compatibility or
                // test_graceful_cluster_restart old ybLight binaries
                // (without endpoint_storage) are present.
                if !storage.bin.exists() {
                    eprintln!(
                        "{} binary not found. Ignore if this is a compatibility test",
                        storage.bin
                    );
                    return Ok(());
                }
                if let Err(e) = storage.start(start_timeout).await {
                    eprintln!("endpoint_storage start failed: {e}");
                    exit(1);
                }
            }
        }
        Stop(EndpointStorageStopCmd { stop_mode }) => {
            let immediate = match stop_mode {
                StopMode::Fast => false,
                StopMode::Immediate => true,
            };
            for es_conf in &env.endpoint_storages {
                let storage = EndpointStorage::from_conf(env, es_conf);
                if let Err(e) = storage.stop(immediate) {
                    eprintln!("endpoint_storage stop failed: {e}");
                    exit(1);
                }
            }
        }
    };
    Ok(())
}

async fn handle_start_all(
    args: &StartCmdArgs,
    env: &'static local_env::LocalEnv,
) -> anyhow::Result<()> {
    // FIXME: this was called "retry_timeout", is it right?
    let Err(errors) = handle_start_all_impl(env, args.timeout).await else {
        yb_light_start_status_check(env, args.timeout.as_ref())
            .await
            .context("status check after successful startup of all services")?;
        return Ok(());
    };

    eprintln!("startup failed because one or more services could not be started");

    for e in errors {
        eprintln!("{e}");
        let debug_repr = format!("{e:?}");
        for line in debug_repr.lines() {
            eprintln!("  {line}");
        }
    }

    try_stop_all(env, true).await;

    exit(2);
}

/// Returns Ok() if and only if all services could be started successfully.
/// Otherwise, returns the list of errors that occurred during startup.
async fn handle_start_all_impl(
    env: &'static local_env::LocalEnv,
    retry_timeout: humantime::Duration,
) -> Result<(), Vec<anyhow::Error>> {
    // Endpoints are not started automatically

    // Phase 1 — provision the external `yb_storage` database
    // synchronously. Just CREATE DATABASE; the schema itself is
    // created by storage_controller's diesel migrations in phase 2.
    if let Err(e) = StorageController::from_env(env)
        .prepare_database_if_external()
        .await
    {
        return Err(vec![
            e.context("prepare external storage controller database"),
        ]);
    }

    // Phase 2 — storage_controller. The controller runs diesel
    // migrations on startup, and pageserver / safekeeper /
    // endpoint_storage all need those tables in place before they
    // can connect to yb_storage, so this phase has to complete
    // before the next one starts.
    if let Err(e) = StorageController::from_env(env)
        .start(YbLightStorageControllerStartArgs::with_default_instance_id(
            retry_timeout,
        ))
        .await
    {
        return Err(vec![e.context("start storage_controller")]);
    }

    // Phase 3 — pageserver(s), safekeeper(s), endpoint_storage in
    // parallel. Schema is guaranteed to exist by this point.
    let mut js = JoinSet::new();

    #[allow(clippy::redundant_closure_call)]
    (|| {
        for ps_conf in &env.pageservers {
            js.spawn(async move {
                let pageserver = PageServerNode::from_env(env, ps_conf);
                // Single-timeline pageserver: the process needs a
                // (tenant, timeline) identity in its pageserver.toml to
                // start. A fresh repo doesn't have one yet — `tenant
                // create` bakes it in and starts the pageserver itself.
                match pageserver.read_timeline_identity() {
                    Ok(None) => {
                        println!(
                            "Skipping pageserver {}: no timeline identity yet \
                             (run `tenant create` to assign one and start it)",
                            ps_conf.id
                        );
                        return Ok(());
                    }
                    Ok(Some(_)) => {}
                    Err(e) => {
                        return Err(e.context(format!(
                            "read timeline identity of pageserver {}",
                            ps_conf.id
                        )));
                    }
                }
                pageserver
                    .start(&retry_timeout)
                    .await
                    .map_err(|e| e.context(format!("start pageserver {}", ps_conf.id)))
            });
        }

        for node in env.safekeepers.iter() {
            js.spawn(async move {
                let safekeeper = SafekeeperNode::from_env(env, node);
                safekeeper
                    .start(&[], &retry_timeout)
                    .await
                    .map_err(|e| e.context(format!("start safekeeper {}", safekeeper.id)))
            });
        }

        for es_conf in &env.endpoint_storages {
            js.spawn(async move {
                EndpointStorage::from_conf(env, es_conf)
                    .start(&retry_timeout)
                    .await
                    .map_err(|e| {
                        e.context(format!(
                            "start endpoint_storage for endpoint {}",
                            es_conf.endpoint_id
                        ))
                    })
            });
        }
    })();

    let mut errors = Vec::new();
    while let Some(result) = js.join_next().await {
        let result = result.expect("we don't panic or cancel the tasks");
        if let Err(e) = result {
            errors.push(e);
        }
    }

    if !errors.is_empty() {
        return Err(errors);
    }

    Ok(())
}

async fn yb_light_start_status_check(
    env: &local_env::LocalEnv,
    retry_timeout: &Duration,
) -> anyhow::Result<()> {
    const RETRY_INTERVAL: Duration = Duration::from_millis(100);
    const NOTICE_AFTER_RETRIES: Duration = Duration::from_secs(5);

    let storcon = StorageController::from_env(env);

    let retries = retry_timeout.as_millis() / RETRY_INTERVAL.as_millis();
    let notice_after_retries = retry_timeout.as_millis() / NOTICE_AFTER_RETRIES.as_millis();

    println!("\nRunning ybLight status check");

    for retry in 0..retries {
        if retry == notice_after_retries {
            println!("\nybLight status check has not passed yet, continuing to wait")
        }

        let mut passed = true;
        let mut nodes = storcon.node_list().await?;
        // Only pageservers with a timeline identity were started (a
        // fresh repo's pageserver starts later, at `tenant create`).
        let mut pageservers = env
            .pageservers
            .iter()
            .filter(|ps_conf| {
                PageServerNode::from_env(env, ps_conf)
                    .read_timeline_identity()
                    .ok()
                    .flatten()
                    .is_some()
            })
            .cloned()
            .collect::<Vec<_>>();

        if nodes.len() < pageservers.len() {
            continue;
        }
        // Extra registered nodes (e.g. a node row surviving from a
        // previous identity) are tolerated; check only the expected ones.
        nodes.retain(|n| pageservers.iter().any(|ps| ps.id == n.id));
        if nodes.len() != pageservers.len() {
            continue;
        }

        nodes.sort_by_key(|ps| ps.id);
        pageservers.sort_by_key(|ps| ps.id);

        for (idx, pageserver) in pageservers.iter().enumerate() {
            let node = &nodes[idx];
            if node.id != pageserver.id {
                passed = false;
                break;
            }

            if !matches!(node.availability, NodeAvailabilityWrapper::Active) {
                passed = false;
                break;
            }
        }

        if passed {
            println!("\nybLight started and passed status check");
            return Ok(());
        }

        tokio::time::sleep(RETRY_INTERVAL).await;
    }

    anyhow::bail!("\nybLight passed status check")
}

async fn handle_stop_all(args: &StopCmdArgs, env: &local_env::LocalEnv) -> Result<()> {
    let immediate = match args.mode {
        StopMode::Fast => false,
        StopMode::Immediate => true,
    };

    try_stop_all(env, immediate).await;

    Ok(())
}

async fn try_stop_all(env: &local_env::LocalEnv, immediate: bool) {
    let mode = if immediate {
        EndpointTerminateMode::Immediate
    } else {
        EndpointTerminateMode::Fast
    };
    // Stop all endpoints
    match ComputeControlPlane::load(env.clone()).await {
        Ok(cplane) => {
            for (_k, node) in cplane.endpoints {
                if let Err(e) = node.stop(mode, false).await {
                    eprintln!("postgres stop failed: {e:#}");
                }
            }
        }
        Err(e) => {
            eprintln!("postgres stop failed, could not restore control plane data from env: {e:#}")
        }
    }

    for es_conf in &env.endpoint_storages {
        let storage = EndpointStorage::from_conf(env, es_conf);
        if let Err(e) = storage.stop(immediate) {
            eprintln!(
                "endpoint_storage for endpoint {} stop failed: {e:#}",
                es_conf.endpoint_id
            );
        }
    }

    for ps_conf in &env.pageservers {
        let pageserver = PageServerNode::from_env(env, ps_conf);
        if let Err(e) = pageserver.stop(immediate) {
            eprintln!("pageserver {} stop failed: {:#}", ps_conf.id, e);
        }
    }

    for node in env.safekeepers.iter() {
        let safekeeper = SafekeeperNode::from_env(env, node);
        if let Err(e) = safekeeper.stop(immediate) {
            eprintln!("safekeeper {} stop failed: {:#}", safekeeper.id, e);
        }
    }

    // Stop all storage controller instances. In the most common case there's only one,
    // but iterate though the base data directory in order to discover the instances.
    let storcon_instances = env
        .storage_controller_instances()
        .await
        .expect("Must inspect data dir");
    for (instance_id, _instance_dir_path) in storcon_instances {
        let storage_controller = StorageController::from_env(env);
        let stop_args = YbLightStorageControllerStopArgs {
            instance_id,
            immediate,
        };

        if let Err(e) = storage_controller.stop(stop_args).await {
            eprintln!("Storage controller instance {instance_id} stop failed: {e:#}");
        }
    }
}

#[cfg(test)]
mod tests {
    use std::path::PathBuf;

    use super::*;

    fn ps_template() -> YbLightLocalInitPageserverConf {
        YbLightLocalInitPageserverConf {
            id: NodeId(1),
            listen_pg_addr: "127.0.0.1:64000".to_string(),
            listen_http_addr: "127.0.0.1:9898".to_string(),
            listen_https_addr: None,
            listen_grpc_addr: Some("127.0.0.1:51051".to_string()),
            pg_auth_type: AuthType::Trust,
            http_auth_type: AuthType::Trust,
            grpc_auth_type: AuthType::Trust,
            no_sync: true,
            remote_storage: Some("{ host = 'h' }".to_string()),
            other: Default::default(),
        }
    }

    fn sk_template() -> SafekeeperConf {
        SafekeeperConf {
            id: NodeId(1),
            pg_port: 5454,
            http_port: 7676,
            wal_yb_config: Some("{ host = 'h' }".to_string()),
            ..Default::default()
        }
    }

    fn env_with_templates() -> LocalEnv {
        LocalEnv {
            base_data_dir: PathBuf::from("/tmp/repo"),
            pg_distrib_dir: PathBuf::from("/pg"),
            yb_light_distrib_dir: PathBuf::from("/yb_light"),
            default_tenant_id: None,
            private_key_path: PathBuf::from("k"),
            public_key_path: PathBuf::from("k.pub"),
            storage_controller: YbLightStorageControllerConf::default(),
            pageservers: Vec::new(),
            safekeepers: Vec::new(),
            pageserver_templates: vec![ps_template()],
            safekeeper_templates: vec![sk_template()],
            endpoint_storage: EndpointStorageConf::default(),
            endpoint_storages: Vec::new(),
            control_plane_api: "http://127.0.0.1:1234/".parse().unwrap(),
            control_plane_hooks_api: None,
            use_local_tls: false,
        }
    }

    #[test]
    fn first_pageserver_reuses_template_id_and_ports() {
        let env = env_with_templates();
        let conf = allocate_pageserver_conf(&env, "main").unwrap();
        assert_eq!(conf.id, NodeId(1));
        assert_eq!(conf.listen_pg_addr, "127.0.0.1:64000");
        assert_eq!(conf.listen_grpc_addr.as_deref(), Some("127.0.0.1:51051"));
        assert_eq!(
            conf.data_dir,
            PathBuf::from("/tmp/repo/endpoints/main/pageserver")
        );
    }

    #[test]
    fn second_pageserver_gets_next_id_and_port_delta() {
        let mut env = env_with_templates();
        env.pageservers
            .push(allocate_pageserver_conf(&env.clone(), "main").unwrap());
        let conf = allocate_pageserver_conf(&env, "ep_b1").unwrap();
        // id 2, delta = (2 - 1) * 2 = 2
        assert_eq!(conf.id, NodeId(2));
        assert_eq!(conf.listen_pg_addr, "127.0.0.1:64002");
        assert_eq!(conf.listen_grpc_addr.as_deref(), Some("127.0.0.1:51053"));
        assert_eq!(
            conf.data_dir,
            PathBuf::from("/tmp/repo/endpoints/ep_b1/pageserver")
        );
    }

    #[test]
    fn first_safekeeper_reuses_template_id_and_ports() {
        let env = env_with_templates();
        let tl = TimelineId::generate();
        let conf = allocate_safekeeper_conf(&env, "main", tl).unwrap();
        assert_eq!(conf.id, NodeId(1));
        assert_eq!(conf.pg_port, 5454);
        assert_eq!(conf.http_port, 7676);
        assert_eq!(conf.timeline_id, Some(tl));
        assert_eq!(conf.endpoint_id, "main");
        assert_eq!(conf.wal_yb_config.as_deref(), Some("{ host = 'h' }"));
    }

    #[test]
    fn second_safekeeper_gets_next_id_and_port_delta() {
        let mut env = env_with_templates();
        let main_tl = TimelineId::generate();
        env.safekeepers
            .push(allocate_safekeeper_conf(&env.clone(), "main", main_tl).unwrap());
        let branch_tl = TimelineId::generate();
        let conf = allocate_safekeeper_conf(&env, "ep_b1", branch_tl).unwrap();
        // id 2, delta = 2
        assert_eq!(conf.id, NodeId(2));
        assert_eq!(conf.pg_port, 5456);
        assert_eq!(conf.http_port, 7678);
        assert_eq!(conf.timeline_id, Some(branch_tl));
        assert_eq!(conf.endpoint_id, "ep_b1");
    }

    #[test]
    fn first_endpoint_storage_reuses_template_port() {
        let env = env_with_templates();
        let conf = allocate_endpoint_storage_conf(&env, "main");
        // Template default port (9993), endpoint_id stamped in.
        assert_eq!(conf.listen_addr.port(), 9993);
        assert_eq!(conf.endpoint_id, "main");
    }

    #[test]
    fn second_endpoint_storage_gets_next_free_port() {
        let mut env = env_with_templates();
        env.endpoint_storages
            .push(allocate_endpoint_storage_conf(&env.clone(), "main"));
        let conf = allocate_endpoint_storage_conf(&env, "ep_b1");
        // Lowest free port of the form base + 2k → 9995.
        assert_eq!(conf.listen_addr.port(), 9995);
        assert_eq!(conf.endpoint_id, "ep_b1");
    }

    #[test]
    fn endpoint_storage_reuses_freed_port_after_deprovision() {
        let mut env = env_with_templates();
        env.endpoint_storages
            .push(allocate_endpoint_storage_conf(&env.clone(), "main"));
        env.endpoint_storages
            .push(allocate_endpoint_storage_conf(&env.clone(), "ep_b1"));
        // Drop the first endpoint's instance (port 9993 freed).
        env.endpoint_storages.retain(|e| e.endpoint_id != "main");
        let conf = allocate_endpoint_storage_conf(&env, "ep_b2");
        assert_eq!(conf.listen_addr.port(), 9993);
    }

    #[test]
    fn pageserver_init_conf_preserves_template_remote_storage() {
        let env = env_with_templates();
        let allocated = allocate_pageserver_conf(&env, "main").unwrap();
        let init_conf = pageserver_init_conf_for(&env, &allocated).unwrap();
        assert_eq!(init_conf.id, allocated.id);
        assert_eq!(init_conf.listen_pg_addr, allocated.listen_pg_addr);
        assert_eq!(init_conf.remote_storage.as_deref(), Some("{ host = 'h' }"));
        assert!(init_conf.no_sync);
    }
}
