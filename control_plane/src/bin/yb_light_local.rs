//!
//! `yb_light_local` is an executable that can be used to create a local
//! ybLight environment, for testing purposes. The local environment is
//! quite different from the cloud environment with Kubernetes, but it
//! easier to work with locally. The python tests in `old_neon_test_runner`
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
use control_plane::endpoint::local_pageserver_conf_to_conn_info;
use control_plane::endpoint::{ComputeControlPlane, EndpointTerminateMode};
use control_plane::endpoint_storage::{ENDPOINT_STORAGE_DEFAULT_ADDR, EndpointStorage};
use control_plane::local_env;
use control_plane::local_env::{
    EndpointStorageConf, InitForceMode, LocalEnv, PageServerConf, SafekeeperConf,
    YbLightLocalInitConf, YbLightLocalInitPageserverConf, YbLightStorageControllerConf,
};
use control_plane::pageserver::PageServerNode;
use control_plane::safekeeper::SafekeeperNode;
use control_plane::storage_controller::{
    StorageController, YbLightStorageControllerStartArgs, YbLightStorageControllerStopArgs,
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
use safekeeper_api::{
    DEFAULT_HTTP_LISTEN_PORT as DEFAULT_SAFEKEEPER_HTTP_PORT,
    DEFAULT_PG_LISTEN_PORT as DEFAULT_SAFEKEEPER_PG_PORT, PgMajorVersion, PgVersionId,
};
use tokio::task::JoinSet;
use utils::auth::{Claims, Scope};
use utils::id::{BranchId, NodeId};
use utils::lsn::Lsn;
use utils::project_git_version;

// Default id of a safekeeper node, if not specified on the command line.
const DEFAULT_SAFEKEEPER_ID: NodeId = NodeId(1);
const DEFAULT_PAGESERVER_ID: NodeId = NodeId(1);
const DEFAULT_BRANCH_NAME: &str = "main";
project_git_version!(GIT_VERSION);

#[allow(dead_code)]
const DEFAULT_PG_VERSION: PgMajorVersion = PgMajorVersion::PG18;
const DEFAULT_PG_VERSION_NUM: &str = "18";

const DEFAULT_PAGESERVER_CONTROL_PLANE_API: &str = "http://127.0.0.1:1234/upcall/v1/";

const DEFAULT_YB_HOST: &str = "127.0.0.1";
const DEFAULT_YB_PORT: u16 = 5433;
const DEFAULT_YB_USER: &str = "yugabyte";
const DEFAULT_YB_DBNAME: &str = "yb_storage";

/// The shared `[*.yb]` connection config for the dev cluster's YB. The
/// pageserver (`yb_storage_conn`), safekeeper (`yb_storage_conn`),
/// endpoint_storage and storage_controller (`--yb-config`) all consume this
/// same block — each hardcodes the table(s) it owns, so it carries none.
fn default_yb_conn_config(target: &YbStoreTarget) -> ysql_client::YbConnConfig {
    let YbStoreTarget {
        host,
        port,
        dbname,
        hot_path,
    } = target;
    ysql_client::YbConnConfig {
        host_ports: format!("{host}:{port}").parse().expect("valid host:port"),
        user: DEFAULT_YB_USER.to_string(),
        dbname: dbname.clone(),
        password_env: None,
        sslmode: None,
        sslrootcert: None,
        disable_transactional_writes: false,
        hot_path: *hot_path,
        tserver_rpc_addrs: None,
    }
}

/// Which YugabyteDB the default (no `--config`) init config points at.
/// Each field falls back to its `DEFAULT_YB_*` constant; `init` overrides
/// them from `--yb-host` / `--yb-port` / `--yb-dbname` / `--yb-hot-path`.
struct YbStoreTarget {
    host: String,
    port: u16,
    dbname: String,
    hot_path: ysql_client::HotPath,
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
    Branch(BranchCmd),
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

    /// YugabyteDB database name baked into the default (no `--config`)
    /// config: the shared `[*.yb]` block consumed by the storage controller
    /// (`--yb-config`), pageserver (`yb_storage_conn`), safekeeper
    /// (`yb_storage_conn`) and endpoint_storage all point at it. Defaults to
    /// `yb_storage`. Lets callers (e.g. the e2e harness)
    /// target an ephemeral per-run database without templating a base TOML.
    /// Ignored when `--config` is given.
    #[clap(long)]
    yb_dbname: Option<String>,

    /// YugabyteDB host for the default (no `--config`) config. Defaults to
    /// `127.0.0.1`. Ignored when `--config` is given.
    #[clap(long)]
    yb_host: Option<String>,

    /// YugabyteDB port for the default (no `--config`) config. Defaults to
    /// `5433`. Ignored when `--config` is given.
    #[clap(long)]
    yb_port: Option<u16>,

    /// Hot-path transport for the default (no `--config`) config:
    /// `tsclient` (default; pure-Rust thin RPC to the tserver RPC port)
    /// or `ysql` (Postgres wire escape hatch). Ignored when `--config` is
    /// given.
    #[clap(long)]
    yb_hot_path: Option<String>,

    /// Relocate the whole default (no `--config`) cluster into a free port
    /// block starting at this base, so it can run alongside another cluster
    /// (e.g. a developer's on the defaults). Each service family gets its
    /// own 100-port lane off the base: control_plane_api = B,
    /// safekeeper pg = B+100 / http = B+200, pageserver pg = B+300 /
    /// http = B+400 / grpc = B+500, endpoint_storage = B+600 (per-instance
    /// deltas stay within the lane). When unset, the usual defaults
    /// (1234 / 5454 / 64000 / 9993 / …) are used. Ignored when `--config`
    /// is given.
    #[clap(long)]
    port_base: Option<u16>,

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
    // exist". See #49
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
    /// Branch ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: Option<BranchId>,

    #[clap(short = 'c')]
    config: Vec<String>,

    /// Postgres version to use for the initial branch.
    #[arg(default_value = DEFAULT_PG_VERSION_NUM)]
    #[clap(long)]
    pg_version: PgMajorVersion,

    /// Use this branch in future CLI commands where branch_id is needed, but not specified.
    #[clap(long)]
    set_default: bool,

    /// Placement policy for this branch.
    #[clap(long)]
    #[arg(value_parser = parse_placement_policy)]
    placement_policy: Option<PlacementPolicy>,
}

fn parse_placement_policy(s: &str) -> anyhow::Result<PlacementPolicy> {
    Ok(serde_json::from_str::<PlacementPolicy>(s)?)
}

/// Set a particular branch as default in future CLI commands where branch_id is needed, but not
/// specified.
#[derive(clap::Args)]
struct TenantSetDefaultCmdArgs {
    /// Branch ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: BranchId,
}

#[derive(clap::Args)]
struct TenantConfigCmdArgs {
    /// Branch ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: Option<BranchId>,

    #[clap(short = 'c')]
    config: Vec<String>,
}

/// Import a branch that is present in remote storage.
#[derive(clap::Args)]
struct TenantImportCmdArgs {
    /// Branch ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: BranchId,
}

/// Manage timelines.
#[derive(clap::Subcommand)]
enum TimelineCmd {
    List(TimelineListCmdArgs),
    Branch(TimelineBranchCmdArgs),
    Drop(TimelineDropCmdArgs),
    Create(TimelineCreateCmdArgs),
    Import(TimelineImportCmdArgs),
}

/// List all branches available to this pageserver.
#[derive(clap::Args)]
struct TimelineListCmdArgs {
    /// Branch ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: Option<BranchId>,
}

/// Drop a branch and all of its endpoints: stop the endpoints' compute
/// processes, delete their records, and mark the branch dropped in the
/// storage controller. The underlying pageserver/safekeeper data is left for a
/// later GC pass.
#[derive(clap::Args)]
struct TimelineDropCmdArgs {
    /// Branch ID of the branch to drop, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: BranchId,
}

/// Create a new branch, branching off from another branch.
#[derive(clap::Args)]
struct TimelineBranchCmdArgs {
    /// New branch's ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: Option<BranchId>,
    /// Human-readable alias for the new branch.
    #[clap(long)]
    branch_name: String,
    /// Use last Lsn of another branch (and its data) as base when creating the new branch. The
    /// branch gets resolved by its branch name.
    #[clap(long)]
    ancestor_branch_name: Option<String>,
    /// When using another branch as base, use a specific Lsn in it instead of the latest one.
    #[clap(long)]
    ancestor_start_lsn: Option<Lsn>,
}

/// Create a new blank branch.
#[derive(clap::Args)]
struct TimelineCreateCmdArgs {
    /// New branch's ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: Option<BranchId>,
    /// Human-readable alias for the new branch.
    #[clap(long)]
    branch_name: String,

    /// Postgres version.
    #[arg(default_value = DEFAULT_PG_VERSION_NUM)]
    #[clap(long)]
    pg_version: PgMajorVersion,
}

/// Import a branch from a basebackup directory.
#[derive(clap::Args)]
struct TimelineImportCmdArgs {
    /// New branch's ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: BranchId,
    /// Human-readable alias for the new branch.
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

/// Manage branches.
#[derive(clap::Subcommand)]
enum BranchCmd {
    Create(BranchCreateCmdArgs),
    Drop(BranchDropCmdArgs),
}

/// Create a branch.
///
/// With no `--ancestor-branch-name`, creates a ROOT branch (no ancestor) —
/// equivalent to the old `tenant create`. With `--ancestor-branch-name`,
/// creates a FORK off the named ancestor — equivalent to the old
/// `timeline branch`.
#[derive(clap::Args)]
struct BranchCreateCmdArgs {
    /// New branch's ID, as a 32-byte hexadecimal string. Generated if omitted.
    #[clap(long = "branch-id")]
    branch_id: Option<BranchId>,

    /// Human-readable alias for the new branch. Defaults to `main` for a root
    /// branch; required in practice for a fork.
    #[clap(long)]
    branch_name: Option<String>,

    /// Name of the ancestor branch to fork from. When set, this creates a fork
    /// branch off the named ancestor; when unset, a root branch is created.
    #[clap(long)]
    ancestor_branch_name: Option<String>,

    /// When forking, use a specific Lsn in the ancestor instead of the latest one.
    #[clap(long)]
    ancestor_start_lsn: Option<Lsn>,

    /// Postgres version to use for a root branch (a fork inherits its
    /// ancestor's version).
    #[arg(default_value = DEFAULT_PG_VERSION_NUM)]
    #[clap(long)]
    pg_version: PgMajorVersion,

    /// Use this branch in future CLI commands where branch_id is needed, but
    /// not specified (root branch only).
    #[clap(long)]
    set_default: bool,
}

/// Drop a branch and all of its endpoints: stop the endpoints' compute
/// processes, delete their records, and mark the branch dropped in the
/// storage controller. The underlying pageserver/safekeeper data is left for a
/// later GC pass.
#[derive(clap::Args)]
struct BranchDropCmdArgs {
    /// Branch ID of the branch to drop, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: BranchId,
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
    Stop(EndpointStopCmdArgs),
    GenerateJwt(EndpointGenerateJwtCmdArgs),
}

/// List endpoints.
#[derive(clap::Args)]
struct EndpointListCmdArgs {
    /// Branch ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: Option<BranchId>,
}

/// Create a compute endpoint.
#[derive(clap::Args)]
struct EndpointCreateCmdArgs {
    /// Branch ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: Option<BranchId>,
    /// Postgres endpoint ID.
    endpoint_id: Option<String>,
    /// Name of the branch the endpoint will run on.
    #[clap(long)]
    branch_name: Option<String>,
    /// Specify LSN on the branch to start from. By default, end of the branch would be used.
    #[clap(long)]
    lsn: Option<Lsn>,
    #[clap(long)]
    pg_port: Option<u16>,
    #[clap(long, alias = "http-port")]
    external_http_port: Option<u16>,
    #[clap(long)]
    internal_http_port: Option<u16>,

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
    /// If set, will set up the catalog for yb_amp_superuser.
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
    /// both `yb_amp.max_file_cache_size` (postmaster) and
    /// `yb_amp.file_cache_size_limit` (SIGHUP) to this value. Pass a
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
    /// List of safekeepers endpoint will talk to.
    #[clap(long)]
    safekeepers: Option<String>,
    /// Configure the remote extensions storage proxy gateway URL to request for extensions.
    #[clap(long, alias = "remote-ext-config")]
    remote_ext_base_url: Option<String>,
    /// If set, will create test user `user` and `ybampdb` database. Requires `update-catalog = true`
    #[clap(long)]
    create_test_user: bool,
    /// Allow multiple primary endpoints running on the same branch. Shouldn't be used normally, but
    /// useful for tests.
    #[clap(long)]
    allow_multiple: bool,
    /// Timeout until we fail the command.
    #[clap(short = 't', long, value_parser= humantime::parse_duration)]
    #[arg(default_value = "180s")]
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
    /// Branch id. Represented as a hexadecimal string 32 symbols length
    #[clap(long = "branch-id")]
    branch_id: Option<BranchId>,
    /// Postgres endpoint ID.
    endpoint_id: String,
    #[clap(long)]
    safekeepers: Option<String>,
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
    /// Branch ID, as a 32-byte hexadecimal string.
    #[clap(long = "branch-id")]
    branch_id: BranchId,
    /// Branch name to give to the branch.
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
    /// Holds all direct children of this branch referenced using `branch_id`.
    pub children: BTreeSet<BranchId>,
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
            YbLightLocalCmd::Branch(subcmd) => rt.block_on(handle_branch(&subcmd, env)),
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
    mut timeline_name_mappings: HashMap<BranchId, String>,
) -> Result<()> {
    let mut timelines_hash = timelines
        .iter()
        .map(|t| {
            (
                t.branch_id,
                TimelineTreeEl {
                    info: t.clone(),
                    children: BTreeSet::new(),
                    name: timeline_name_mappings.remove(&t.branch_id),
                },
            )
        })
        .collect::<HashMap<_, _>>();

    // Memorize all direct children of each branch.
    for timeline in timelines.iter() {
        if let Some(ancestor_branch_id) = timeline.ancestor_branch_id {
            timelines_hash
                .get_mut(&ancestor_branch_id)
                .context("missing branch info in the HashMap")?
                .children
                .insert(timeline.branch_id);
        }
    }

    for timeline in timelines_hash.values() {
        // Start with root local branches (no ancestors) first.
        if timeline.info.ancestor_branch_id.is_none() {
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
    timelines: &HashMap<BranchId, TimelineTreeEl>,
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
        timeline.info.branch_id
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

/// Helper function to get branch id from an optional --branch-id option or from the config file
fn get_tenant_id(
    branch_id_arg: Option<BranchId>,
    env: &local_env::LocalEnv,
) -> anyhow::Result<BranchId> {
    if let Some(branch_id_from_arguments) = branch_id_arg {
        Ok(branch_id_from_arguments)
    } else if let Some(default_id) = env.default_branch_id {
        Ok(default_id)
    } else {
        anyhow::bail!("No branch id. Use --branch-id, or set a default branch");
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
        let yb_target = YbStoreTarget {
            host: args
                .yb_host
                .clone()
                .unwrap_or_else(|| DEFAULT_YB_HOST.to_string()),
            port: args.yb_port.unwrap_or(DEFAULT_YB_PORT),
            dbname: args
                .yb_dbname
                .clone()
                .unwrap_or_else(|| DEFAULT_YB_DBNAME.to_string()),
            hot_path: match args.yb_hot_path.as_deref() {
                None | Some("tsclient") => ysql_client::HotPath::TsClient,
                Some("ysql") => ysql_client::HotPath::Ysql,
                Some(other) => {
                    anyhow::bail!("--yb-hot-path must be ysql or tsclient, got {other:?}")
                }
            },
        };
        // Port layout. With `--port-base B` the whole cluster moves into a
        // free block — one 100-port lane per service family — so it can run
        // alongside another cluster on the defaults. Without it, each family
        // keeps its usual default. Per-instance deltas (added by allocate_*
        // at `endpoint create`) stay inside the lane.
        let pb = args.port_base;
        let sk_pg_port = pb.map_or(DEFAULT_SAFEKEEPER_PG_PORT, |b| b + 100);
        let sk_http_port = pb.map_or(DEFAULT_SAFEKEEPER_HTTP_PORT, |b| b + 200);
        let ps_pg_port = pb.map_or(DEFAULT_PAGESERVER_PG_PORT, |b| b + 300);
        let ps_http_port = pb.map_or(DEFAULT_PAGESERVER_HTTP_PORT, |b| b + 400);
        let ps_grpc_port = pb.map_or(DEFAULT_PAGESERVER_GRPC_PORT, |b| b + 500);
        let es_port = pb.map_or(ENDPOINT_STORAGE_DEFAULT_ADDR.port(), |b| b + 600);
        // control_plane_api lives at the base (B); default keeps :1234.
        let control_plane_api = match pb {
            Some(b) => format!("http://127.0.0.1:{b}/upcall/v1/").parse().unwrap(),
            None => DEFAULT_PAGESERVER_CONTROL_PLANE_API.parse().unwrap(),
        };
        // One shared YB connection block, emitted identically for every
        // component (the table each owns is hardcoded, not in the config).
        let yb_conn = default_yb_conn_config(&yb_target);
        YbLightLocalInitConf {
            control_plane_api: Some(control_plane_api),
            safekeepers: vec![SafekeeperConf {
                id: DEFAULT_SAFEKEEPER_ID,
                pg_port: sk_pg_port,
                http_port: sk_http_port,
                // Mirror live WAL into <db>.safekeeper.wals.
                yb_storage_conn: Some(yb_conn.to_inline_toml()),
                ..Default::default()
            }],
            pageservers: (0..args.num_pageservers.unwrap_or(1))
                .map(|i| {
                    let pageserver_id = NodeId(DEFAULT_PAGESERVER_ID.0 + i as u64);
                    let pg_port = ps_pg_port + i;
                    let http_port = ps_http_port + i;
                    let grpc_port = ps_grpc_port + i;
                    YbLightLocalInitPageserverConf {
                        id: pageserver_id,
                        listen_pg_addr: format!("127.0.0.1:{pg_port}"),
                        listen_http_addr: format!("127.0.0.1:{http_port}"),
                        listen_https_addr: None,
                        listen_grpc_addr: Some(format!("127.0.0.1:{grpc_port}")),
                        pg_auth_type: AuthType::Trust,
                        http_auth_type: AuthType::Trust,
                        grpc_auth_type: AuthType::Trust,
                        // Pages live in <db>.pageserver.*; relfile/layer
                        // objects in <db>.public.blobs (both tables hardcoded).
                        yb_storage_conn: Some(yb_conn.to_inline_toml()),
                        other: Default::default(),
                        // Typical developer machines use disks with slow fsync, and we don't care
                        // about data integrity: disable disk syncs.
                        no_sync: true,
                    }
                })
                .collect(),
            endpoint_storage: EndpointStorageConf {
                listen_addr: {
                    let mut addr = ENDPOINT_STORAGE_DEFAULT_ADDR;
                    addr.set_port(es_port);
                    addr
                },
                remote_storage_json: None,
                endpoint_id: String::new(),
            },
            pg_distrib_dir: None,
            yb_light_distrib_dir: None,
            default_branch_id: BranchId::from_array(std::array::from_fn(|_| 0)),
            storage_controller: Some(YbLightStorageControllerConf {
                // Provision the storage_controller + storage DBs in YB.
                yb: Some(yb_conn.clone()),
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
        branch_id: None,
        data_dir: env.pageserver_data_dir(endpoint_id),
    })
}

/// Build the full `YbLightLocalInitPageserverConf` (carrying
/// `yb_storage_conn` + other passthrough keys) for the first/main
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
        yb_storage_conn: template.yb_storage_conn.clone(),
        other: template.other.clone(),
    })
}

/// Allocate the conf for a safekeeper to be provisioned under `endpoint_id`,
/// dedicated to `branch_id`. Like [`allocate_pageserver_conf`], the first
/// safekeeper reuses the lowest-id template's id/ports; subsequent ones get
/// the next free id and a per-id port offset.
fn allocate_safekeeper_conf(
    env: &LocalEnv,
    endpoint_id: &str,
    branch_id: BranchId,
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
        yb_storage_conn: template.yb_storage_conn.clone(),
        wal_cache_size_mib: template.wal_cache_size_mib,
        branch_id: Some(branch_id),
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

/// The ancestor branch's durable WAL end (`flush_lsn`), read from its
/// safekeeper. This is the authoritative "everything committed so far"
/// position; the ancestor pageserver's `last_record_lsn` only reflects
/// what its walreceiver has ingested and may lag this by seconds.
async fn ancestor_durable_lsn(env: &LocalEnv, branch_id: BranchId) -> anyhow::Result<Lsn> {
    let sk_conf = env
        .safekeepers_for_timeline(branch_id)
        .into_iter()
        .next()
        .with_context(|| format!("no safekeeper serves branch {branch_id}"))?
        .clone();
    let sk = SafekeeperNode::from_env(env, &sk_conf);
    let status: safekeeper_api::models::TimelineStatus = sk
        .http_client
        .timeline_status(branch_id)
        .await?
        .json()
        .await
        .context("decode safekeeper branch status")?;
    Ok(status.flush_lsn)
}

/// The pageserver CLI branch operations target: the first-configured
/// node, which serves the main branch. Child branches get
/// their own pageservers, resolved via `pageserver_conf_for_timeline`.
fn get_default_pageserver(env: &local_env::LocalEnv) -> anyhow::Result<PageServerNode> {
    let ps_conf = env
        .pageservers
        .first()
        .context("no pageserver provisioned yet — create an endpoint (`endpoint create`) first")?;
    Ok(PageServerNode::from_env(env, ps_conf))
}

async fn handle_tenant(subcmd: &TenantCmd, env: &mut local_env::LocalEnv) -> anyhow::Result<()> {
    match subcmd {
        TenantCmd::List => {
            for ps_conf in &env.pageservers {
                let ps = PageServerNode::from_env(env, ps_conf);
                match ps.read_timeline_identity()? {
                    Some(branch_id) => {
                        println!("branch {branch_id} (pageserver {})", ps_conf.id)
                    }
                    None => println!("(pageserver {}: no branch assigned)", ps_conf.id),
                }
            }
        }
        TenantCmd::Import(args) => {
            let branch_id = args.branch_id;

            let storage_controller = StorageController::from_env(env);
            let create_response = storage_controller.branch_import(branch_id).await?;

            let shard_zero = create_response
                .shards
                .first()
                .expect("Import response omitted shards");

            let attached_pageserver_id = shard_zero.node_id;

            println!(
                "Imported branch {branch_id}, attached to pageserver {attached_pageserver_id}"
            );

            env.register_branch_mapping("main".to_string(), branch_id)
                .await?;
        }
        TenantCmd::Create(args) => {
            // Effective tenant config is the [tenant_config] section of
            // pageserver.toml; there is no per-tenant override store.
            anyhow::ensure!(
                args.config.is_empty(),
                "per-tenant config overrides are not supported: set [tenant_config] in \
                 pageserver.toml instead"
            );

            // If branch ID was not specified, generate one
            let branch_id = args.branch_id.unwrap_or_else(BranchId::generate);

            // Co-located topology: the branch's PS+SK pair is provisioned
            // under the first endpoint created for this branch, not here.
            // `tenant create` just allocates the id and records the pending
            // Bootstrap recipe in the storage controller (durable cluster
            // state in YB) so `endpoint create` knows to run initdb + import.
            // The `main` branch mapping is registered with the controller
            // only after the branch exists (during provisioning) — the
            // controller rejects a branch name for a branch it doesn't
            // know yet, so until then the pending row carries the name.
            let sc = StorageController::from_env(env);
            sc.record_pending_timeline_init(
                branch_id,
                pageserver_api::controller_api::PendingTimelineInitRequest {
                    branch_name: DEFAULT_BRANCH_NAME.to_string(),
                    pg_version: args.pg_version,
                    ancestor_branch_id: None,
                    start_lsn: Lsn(0),
                },
            )
            .await?;

            // NB: the e2e harness extracts the branch id from this exact line
            // (it finds the line containing "successfully created" and takes
            // the 2nd whitespace-separated token as the branch id). The
            // pageserver that serves it comes up at `endpoint create`.
            println!("branch {branch_id} successfully created");
            println!("Created an initial branch '{branch_id}'");

            if args.set_default {
                println!("Setting branch {branch_id} as a default one");
                env.default_branch_id = Some(branch_id);
            }
        }
        TenantCmd::SetDefault(args) => {
            println!("Setting branch {} as a default one", args.branch_id);
            env.default_branch_id = Some(args.branch_id);
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
            let _branch_id = get_tenant_id(args.branch_id, env)?;
            let timelines = pageserver.timeline_list().await?;
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
            let new_branch_id = args.branch_id.unwrap_or_else(BranchId::generate);
            let new_branch_name = &args.branch_name;
            let ancestor_branch_name = args
                .ancestor_branch_name
                .clone()
                .unwrap_or(DEFAULT_BRANCH_NAME.to_owned());
            let ancestor_branch_id = env
                .get_branch_timeline_id(&ancestor_branch_name)
                .await?
                .ok_or_else(|| {
                    anyhow!("Found no branch id for branch name '{ancestor_branch_name}'")
                })?;

            // Co-located topology: a branch is a NEW pageserver +
            // safekeeper pair, but they aren't provisioned here — that
            // happens under the first endpoint created for the branch
            // (`endpoint create`). Here we only capture the branch point
            // and pg_version from the ancestor (which is running now) and
            // record a pending Branch init. The branch's pre-branch data
            // stays under the ancestor's branch_id in YB and is served
            // via lineage-aware reads — nothing is copied.
            let ancestor_ps_conf = env
                .pageserver_conf_for_timeline(ancestor_branch_id)
                .with_context(|| {
                    format!("no pageserver serves ancestor branch {ancestor_branch_id}")
                })?
                .clone();
            let ancestor_ps = PageServerNode::from_env(env, &ancestor_ps_conf);
            let ancestor_info = ancestor_ps
                .timeline_info(
                    ancestor_branch_id,
                    pageserver_client::mgmt_api::ForceAwaitLogicalSize::No,
                )
                .await
                .context("fetch ancestor branch info")?;
            let pg_version = ancestor_info.pg_version;

            // Default branch point: the ancestor's durable WAL end, from
            // its safekeeper. The pageserver's last_record_lsn is its
            // ingest position, which can lag the compute's flushed WAL
            // by an arbitrary amount — branching there would cut a
            // recently-committed transaction out of the branch.
            let start_lsn = match args.ancestor_start_lsn {
                Some(lsn) => lsn,
                None => ancestor_durable_lsn(env, ancestor_branch_id)
                    .await
                    .context("fetch ancestor durable WAL end from safekeeper")?,
            };

            // The branch-name mapping is registered with the storage
            // controller only after the branch exists (during
            // provisioning); until then the pending row carries the branch
            // name so `endpoint create` can resolve it.
            let sc = StorageController::from_env(env);
            sc.record_pending_timeline_init(
                new_branch_id,
                pageserver_api::controller_api::PendingTimelineInitRequest {
                    branch_name: new_branch_name.to_string(),
                    pg_version,
                    ancestor_branch_id: Some(ancestor_branch_id),
                    start_lsn,
                },
            )
            .await?;

            println!(
                "Created branch '{new_branch_name}' (branch {new_branch_id}) at {start_lsn} \
                 from '{ancestor_branch_name}'; its pageserver + safekeeper come up at \
                 `endpoint create`",
            );
        }
        TimelineCmd::Drop(args) => {
            let branch_id = args.branch_id;

            // Stop + tear down every endpoint sitting on this branch. With
            // `destroy`, `stop` halts the compute process, deprovisions the
            // co-located pageserver/safekeeper/endpoint_storage, removes the
            // local endpoint dir, and deletes the endpoint's record from the
            // storage controller.
            let cplane = ComputeControlPlane::load(env.clone()).await?;
            let to_drop: Vec<_> = cplane
                .endpoints
                .iter()
                .filter(|(_, ep)| ep.branch_id == branch_id)
                .map(|(id, ep)| (id.clone(), ep.clone()))
                .collect();

            for (endpoint_id, endpoint) in &to_drop {
                endpoint
                    .stop(EndpointTerminateMode::Fast, /* destroy */ true)
                    .await
                    .with_context(|| format!("stopping endpoint {endpoint_id}"))?;
            }

            // Durable cleanup in the storage controller: delete any endpoint
            // records still present, discard an un-materialized init recipe,
            // and mark the branch dropped so it no longer surfaces as a live
            // branch and isn't brought back on restart. Idempotent.
            StorageController::from_env(env)
                .timeline_drop(branch_id)
                .await?;

            println!(
                "Dropped branch (branch {branch_id}) and {} endpoint(s); the branch is \
                 marked dropped — its pageserver/safekeeper data is left for a later GC pass",
                to_drop.len(),
            );
        }
    }

    Ok(())
}

/// Handle the `branch` subcommands, the branch_id-native command surface the
/// e2e harness drives. These reshape onto the existing root-create / fork /
/// drop logic:
///
/// * `branch create` with no `--ancestor-branch-name` creates a ROOT branch
///   (no ancestor) — the same work as the old `tenant create`.
/// * `branch create --ancestor-branch-name <ANC>` creates a FORK off the
///   named ancestor — the same work as the old `timeline branch`.
/// * `branch drop --branch-id <ID>` drops the branch — the same work as the
///   old `timeline drop`.
async fn handle_branch(subcmd: &BranchCmd, env: &mut local_env::LocalEnv) -> anyhow::Result<()> {
    match subcmd {
        BranchCmd::Create(args) => {
            if args.ancestor_branch_name.is_some() {
                // FORK: reuse the `timeline branch` logic. A fork needs a
                // human-readable name to resolve it later at `endpoint create`.
                let branch_name = args.branch_name.clone().ok_or_else(|| {
                    anyhow!("`branch create` with --ancestor-branch-name requires --branch-name")
                })?;
                let new_branch_id = args.branch_id.unwrap_or_else(BranchId::generate);
                handle_timeline(
                    &TimelineCmd::Branch(TimelineBranchCmdArgs {
                        branch_id: Some(new_branch_id),
                        branch_name,
                        ancestor_branch_name: args.ancestor_branch_name.clone(),
                        ancestor_start_lsn: args.ancestor_start_lsn,
                    }),
                    env,
                )
                .await?;
                // Keep the success-message invariant (2nd token is the branch
                // id) consistent across root and fork creates.
                println!("branch {new_branch_id} successfully created");
            } else {
                // ROOT: reuse the `tenant create` logic, which prints the
                // `branch <id> successfully created` line the harness parses.
                handle_tenant(
                    &TenantCmd::Create(TenantCreateCmdArgs {
                        branch_id: args.branch_id,
                        config: Vec::new(),
                        pg_version: args.pg_version,
                        set_default: args.set_default,
                        placement_policy: None,
                    }),
                    env,
                )
                .await?;
            }
        }
        BranchCmd::Drop(args) => {
            handle_timeline(
                &TimelineCmd::Drop(TimelineDropCmdArgs {
                    branch_id: args.branch_id,
                }),
                env,
            )
            .await?;
        }
    }

    Ok(())
}

/// Provision the pageserver + safekeeper pair that serves `branch_id`,
/// co-located under `endpoint_id`, consuming the pending init recorded at
/// `tenant create` / `timeline branch`. No-op when a pageserver already
/// serves the branch (a second endpoint on an already-served branch —
/// the pair stays under the first endpoint that created it).
async fn provision_timeline_for_endpoint(
    env: &mut LocalEnv,
    endpoint_id: &str,
    branch_id: BranchId,
) -> anyhow::Result<()> {
    if env.pageserver_conf_for_timeline(branch_id).is_some() {
        return Ok(());
    }
    // Read the recipe from the storage controller (durable cluster state in
    // YB). Scoped so the borrow of `env` ends before the `&mut env`
    // provisioning calls below.
    let pending = {
        let sc = StorageController::from_env(env);
        sc.list_pending_timeline_inits()
            .await?
            .into_iter()
            .find(|p| p.branch_id == branch_id)
            .with_context(|| {
                format!(
                    "branch {branch_id} has no provisioned pageserver and no pending init; \
                     run `tenant create` (for the main branch) or `timeline branch` first"
                )
            })?
    };

    // The branch alias is set on the branch row in the same write as
    // `timeline_register` (inside provision_*), folding the old separate
    // set-branch-name round-trip.
    let branch_name = Some(pending.branch_name);
    match pending.ancestor_branch_id {
        // No ancestor => Bootstrap (the initial branch).
        None => {
            provision_bootstrap(env, endpoint_id, branch_id, pending.pg_version, branch_name)
                .await?;
        }
        Some(ancestor_branch_id) => {
            provision_branch(
                env,
                endpoint_id,
                branch_id,
                ancestor_branch_id,
                pending.start_lsn,
                pending.pg_version,
                branch_name,
            )
            .await?;
        }
    }

    // The pair is materialized; drop the pending recipe from YB.
    StorageController::from_env(env)
        .delete_pending_timeline_init(branch_id)
        .await?;
    Ok(())
}

/// Bring up the main branch's pair: start the safekeeper, then start the
/// pageserver (which runs initdb + import to bootstrap the branch), then
/// have the storage controller create the timeline on the safekeeper.
async fn provision_bootstrap(
    env: &mut LocalEnv,
    endpoint_id: &str,
    branch_id: BranchId,
    pg_version: PgMajorVersion,
    branch_name: Option<String>,
) -> anyhow::Result<()> {
    let ps_conf = allocate_pageserver_conf(env, endpoint_id)?;
    let sk_conf = allocate_safekeeper_conf(env, endpoint_id, branch_id)?;
    // Record the safekeeper before building the pageserver toml: its
    // `safekeeper_pg_addr` is taken from `env.safekeepers.first()`.
    env.safekeepers.push(sk_conf.clone());

    // Safekeeper first: it must be up before we create the timeline on it.
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
        branch_id,
        0, // placeholder; start() bumps via attach_hook
        Some(&pageserver_api::config::TimelineInitConfig::Bootstrap { pg_version }),
    )?;
    pageserver
        .register_and_attach()
        .await
        .context("register branch with storage controller")?;
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
                branch_id,
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
                    "bootstrap branch did not become Active in time (state: {:?})",
                    info.state
                );
            }
            Err(e) => {
                return Err(e.context("bootstrap branch did not become Active in time"));
            }
        }
    };

    // Create the timeline on its co-located safekeeper with an explicit
    // single-member configuration, pinning placement to this pair. The
    // storage controller's global safekeeper scheduler would otherwise
    // pick any registered safekeeper — wrong in the co-located topology
    // once more than one pair exists (the compute talks to its own
    // safekeeper). Mirrors `provision_branch`.
    safekeeper
        .create_timeline(&safekeeper_api::models::TimelineCreateRequest {
            branch_id,
            pg_version: PgVersionId::from(pg_version),
            system_id: None,
            wal_seg_size: None,
            start_lsn,
            commit_lsn: None,
        })
        .await
        .context("create bootstrap timeline on safekeeper")?;

    // Record the launcher-created branch in the storage controller (the
    // branch's single co-located safekeeper is owned by the launcher; the
    // controller just tracks the branch row).
    storage_controller
        .timeline_register(
            branch_id,
            pageserver_api::controller_api::TimelineRegisterRequest {
                start_lsn,
                branch_name,
            },
        )
        .await
        .context("register bootstrap branch with storage controller")?;

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
    branch_id: BranchId,
    ancestor_branch_id: BranchId,
    start_lsn: Lsn,
    pg_version: PgMajorVersion,
    branch_name: Option<String>,
) -> anyhow::Result<()> {
    let timer = PhaseTimer::new("branch");
    let ancestor_ps_conf = env
        .pageserver_conf_for_timeline(ancestor_branch_id)
        .with_context(|| format!("no pageserver serves ancestor branch {ancestor_branch_id}"))?
        .clone();
    let ancestor_ps = PageServerNode::from_env(env, &ancestor_ps_conf);

    let ps_conf = allocate_pageserver_conf(env, endpoint_id)?;
    let sk_conf = allocate_safekeeper_conf(env, endpoint_id, branch_id)?;
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
    timer.tick("safekeeper started");
    safekeeper
        .create_timeline(&safekeeper_api::models::TimelineCreateRequest {
            branch_id,
            pg_version: PgVersionId::from(pg_version),
            system_id: None,
            wal_seg_size: None,
            start_lsn,
            commit_lsn: None,
        })
        .await
        .context("create branch timeline on safekeeper")?;
    timer.tick("safekeeper create_timeline");

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
        branch_id,
        0, // placeholder; start() bumps via attach_hook
        Some(&pageserver_api::config::TimelineInitConfig::Branch {
            ancestor_branch_id,
            ancestor_lsn: start_lsn,
        }),
    )?;
    // Best-effort: flush the ancestor's pages to YB up to the branch point
    // *now* (waiting for ingest to reach `start_lsn` first), so the branch
    // pageserver's `create_branch_row` durability barrier (which refuses
    // while `ancestor.consistent_lsn < branch_lsn`) clears on its first try
    // instead of waiting up to one ancestor flush interval (~5s). The
    // branch-PS startup retry remains the correctness guarantee, so a
    // failed/partial flush only costs latency, never safety.
    if let Err(e) = ancestor_ps.flush(ancestor_branch_id, Some(start_lsn)).await {
        println!("branch: best-effort ancestor flush failed (relying on startup retry): {e:#}");
    }
    timer.tick("ancestor flush");

    pageserver
        .start(&std::time::Duration::from_secs(60))
        .await
        .context("start branch pageserver")?;
    timer.tick("pageserver started");

    // Wait for the branch to come up Active.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(120);
    loop {
        match pageserver
            .timeline_info(
                branch_id,
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
                    "branch did not become Active in time (state: {:?})",
                    info.state
                );
            }
            Err(e) => {
                return Err(e.context("branch did not become Active in time"));
            }
        }
    }

    timer.tick("branch Active");
    // Record the launcher-created branch in the storage controller's
    // bookkeeping (branch names + deletion flows hang off the branch row).
    let storage_controller = StorageController::from_env(env);
    storage_controller
        .timeline_register(
            branch_id,
            pageserver_api::controller_api::TimelineRegisterRequest {
                start_lsn,
                branch_name,
            },
        )
        .await
        .context("register branch with storage controller")?;

    env.pageservers.push(ps_conf);
    Ok(())
}

/// Opt-in (`YBL_TIMING=1`) phase timer for one-shot CLI flows: prints
/// elapsed-since-creation at each [`PhaseTimer::tick`], tagged with a scope.
/// Off (and free) by default.
///
/// Hand-rolled deliberately: `std::time::Instant` is the timing primitive,
/// the workspace vendors no lightweight labelled-stopwatch crate, and the
/// idiomatic `tracing`-span approach needs a subscriber wired into the binary
/// (this CLI has none) and times whole spans, not the mid-function
/// checkpoints we want here.
struct PhaseTimer {
    start: std::time::Instant,
    scope: &'static str,
    enabled: bool,
}

impl PhaseTimer {
    fn new(scope: &'static str) -> Self {
        Self {
            start: std::time::Instant::now(),
            scope,
            enabled: std::env::var_os("YBL_TIMING").is_some(),
        }
    }

    /// Print `[timing <scope> +<elapsed>ms] <label>` when `YBL_TIMING` is set.
    fn tick(&self, label: &str) {
        if self.enabled {
            eprintln!(
                "[timing {:<8} +{:>7.1}ms] {label}",
                self.scope,
                self.start.elapsed().as_secs_f64() * 1000.0
            );
        }
    }
}

async fn handle_endpoint(subcmd: &EndpointCmd, env: &mut local_env::LocalEnv) -> Result<()> {
    // Opt-in phase timing (`YBL_TIMING=1`): prints elapsed-since-entry at key
    // checkpoints so `endpoint create` latency can be attributed without a
    // profiler. Cheap and off by default.
    let timer = PhaseTimer::new("endpoint");

    let mut cplane = ComputeControlPlane::load(env.clone()).await?;
    timer.tick("ComputeControlPlane::load");

    match subcmd {
        EndpointCmd::List(args) => {
            // TODO(sharding): this command shouldn't have to specify a shard ID: we should ask the storage controller
            // where shard 0 is attached, and query there.
            let branch_id = get_tenant_id(args.branch_id, env)?;

            let timeline_name_mappings = env.timeline_name_mappings().await?;

            let mut table = comfy_table::Table::new();

            table.load_preset(comfy_table::presets::NOTHING);

            table.set_header([
                "ENDPOINT",
                "ADDRESS",
                "BRANCH",
                "BRANCH NAME",
                "LSN",
                "STATUS",
            ]);

            for (endpoint_id, endpoint) in cplane
                .endpoints
                .iter()
                .filter(|(_, endpoint)| endpoint.branch_id == branch_id)
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
                    .get(&endpoint.branch_id)
                    .map(|name| name.as_str())
                    .unwrap_or("?");

                table.add_row([
                    endpoint_id.as_str(),
                    &endpoint.pg_address.to_string(),
                    &endpoint.branch_id.to_string(),
                    branch_name,
                    lsn_str.as_str(),
                    &format!("{}", endpoint.status()),
                ]);
            }

            println!("{table}");
        }
        EndpointCmd::Create(args) => {
            let branch_name = args
                .branch_name
                .clone()
                .unwrap_or(DEFAULT_BRANCH_NAME.to_owned());
            let endpoint_id = args
                .endpoint_id
                .clone()
                .unwrap_or_else(|| format!("ep-{branch_name}"));

            // Resolve the branch to its branch id. An explicit `--branch-id`
            // wins. Otherwise resolve by name: a provisioned branch is known
            // via its registered mapping; an as-yet-unprovisioned one
            // (recorded by `branch create`, mapping not registered yet) is
            // found in the controller's pending-init rows by name.
            let branch_id = match args.branch_id {
                Some(id) => id,
                None => match env.get_branch_timeline_id(&branch_name).await? {
                    Some(tid) => tid,
                    None => StorageController::from_env(env)
                        .list_pending_timeline_inits()
                        .await?
                        .into_iter()
                        .find(|p| p.branch_name == branch_name)
                        .map(|p| p.branch_id)
                        .ok_or_else(|| {
                            anyhow!("Found no branch id for branch name '{branch_name}'")
                        })?,
                },
            };

            timer.tick("branch resolved");

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
                cplane.check_conflicting_endpoints(mode, branch_id)?;
            }

            // Provision the branch's pageserver + safekeeper pair (one per
            // branch, run the pending Bootstrap/Branch init from `tenant
            // create` / `timeline branch`; a second endpoint on an
            // already-served branch makes this a no-op) CONCURRENTLY with this
            // endpoint's own endpoint_storage instance.
            //
            // The pair is internally ordered and can't be split — the
            // safekeeper must hold the timeline before the pageserver's
            // walreceiver and the compute's walproposer dial in. But
            // endpoint_storage is independent of the pair (per-endpoint LFC
            // scratch store), so overlapping its process start with the
            // longer pair provisioning takes it off the critical path.
            //
            // endpoint_storage's port is reserved up front (serial,
            // `&LocalEnv` read) so it can't race the pair's allocations; its
            // conf is recorded in `env` only after a successful start, and a
            // failed pair provisioning stops it so it can't leak / squat its
            // port.
            let es_conf = allocate_endpoint_storage_conf(env, &endpoint_id);
            let es = EndpointStorage::from_conf(env, &es_conf);
            let es_present = es.bin.exists();
            if es_present {
                println!(
                    "Provisioning endpoint_storage ({}) for endpoint '{endpoint_id}'",
                    es_conf.listen_addr
                );
                es.init().context("endpoint_storage init")?;
            } else {
                eprintln!(
                    "{} binary not found; skipping endpoint_storage provisioning \
                     (ignore if this is a compatibility test)",
                    es.bin
                );
            }

            timer.tick("pre-provision (endpoint_storage init done)");

            let (timeline_res, es_res) = tokio::join!(
                provision_timeline_for_endpoint(env, &endpoint_id, branch_id),
                async {
                    if es_present {
                        es.start(&std::time::Duration::from_secs(30))
                            .await
                            .context("start endpoint_storage")
                    } else {
                        Ok(())
                    }
                },
            );
            match (timeline_res, es_res) {
                (Ok(()), Ok(())) => {
                    if es_present {
                        env.endpoint_storages.push(es_conf);
                    }
                }
                (Err(pair_err), es_res) => {
                    // Pair failed: stop the endpoint_storage we may have
                    // started so it doesn't leak / squat its port, then
                    // surface the original (pair) error.
                    if es_present && es_res.is_ok() {
                        let _ = es.stop(true);
                    }
                    return Err(pair_err);
                }
                (Ok(()), Err(es_err)) => return Err(es_err),
            }
            timer.tick("provision pair ‖ endpoint_storage");

            cplane
                .new_endpoint(
                    &endpoint_id,
                    branch_id,
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
            timer.tick("new_endpoint (record + endpoint_create)");
        }
        EndpointCmd::Start(args) => {
            let endpoint_id = &args.endpoint_id;
            let remote_ext_base_url = &args.remote_ext_base_url;

            let endpoint = cplane
                .endpoints
                .get(endpoint_id.as_str())
                .ok_or_else(|| anyhow!("endpoint {endpoint_id} not found"))?;

            // If --safekeepers argument is given, use only the listed
            // safekeeper nodes; otherwise the safekeepers serving this
            // endpoint's branch (its dedicated one for branches, the
            // legacy set for the main branch).
            let safekeepers = if let Some(safekeepers) = parse_safekeepers(&args.safekeepers)? {
                safekeepers
            } else {
                env.safekeepers_for_timeline(endpoint.branch_id)
                    .iter()
                    .map(|sk| sk.id)
                    .collect()
            };

            if !args.allow_multiple {
                cplane.check_conflicting_endpoints(endpoint.mode, endpoint.branch_id)?;
            }

            let prefer_protocol = if endpoint.grpc {
                PageserverProtocol::Grpc
            } else {
                PageserverProtocol::Libpq
            };

            // Per-endpoint pageserver topology: the pageserver
            // serving this endpoint's branch is identified by
            // the identity in its pageserver.toml.
            let conf = env
                .pageserver_conf_for_timeline(endpoint.branch_id)
                .with_context(|| format!("no pageserver serves branch {}", endpoint.branch_id))?;

            // Initialize an LSN lease for static computes.
            if let ComputeMode::Static(lsn) = endpoint.mode {
                let pageserver = PageServerNode::from_env(env, conf);
                pageserver
                    .http_client
                    .branch_init_lsn_lease(endpoint.branch_id, lsn)
                    .await?;
            }

            let mut pageserver_conninfo =
                local_pageserver_conf_to_conn_info(conf, env.use_local_tls)?;
            pageserver_conninfo.prefer_protocol = prefer_protocol;

            let ps_conf = env.get_pageserver_conf(DEFAULT_PAGESERVER_ID)?;
            let auth_token = if matches!(ps_conf.pg_auth_type, AuthType::YbLightJWT) {
                let claims = Claims::new(Some(endpoint.branch_id), Scope::Tenant);

                Some(env.generate_auth_token(&claims)?)
            } else {
                None
            };

            let exp = (std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH)?
                + Duration::from_secs(86400))
            .as_secs();
            let claims = endpoint_storage::claims::EndpointStorageClaims {
                branch_id: endpoint.branch_id,
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
            let conf = env
                .pageserver_conf_for_timeline(endpoint.branch_id)
                .with_context(|| format!("no pageserver serves branch {}", endpoint.branch_id))?;
            let mut pageserver_conninfo =
                local_pageserver_conf_to_conn_info(conf, env.use_local_tls)?;
            pageserver_conninfo.prefer_protocol = prefer_protocol;

            // If --safekeepers argument is given, use only the listed
            // safekeeper nodes; otherwise all from the env.
            let safekeepers = parse_safekeepers(&args.safekeepers)?;
            endpoint
                .reconfigure(Some(&pageserver_conninfo), safekeepers)
                .await?;
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
            env.register_branch_mapping(args.branch_name.to_owned(), args.branch_id)
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
            yb_storage_conn: Some("{ host = 'h' }".to_string()),
            other: Default::default(),
        }
    }

    fn sk_template() -> SafekeeperConf {
        SafekeeperConf {
            id: NodeId(1),
            pg_port: 5454,
            http_port: 7676,
            yb_storage_conn: Some("{ host = 'h' }".to_string()),
            ..Default::default()
        }
    }

    fn env_with_templates() -> LocalEnv {
        LocalEnv {
            base_data_dir: PathBuf::from("/tmp/repo"),
            http_client: local_env::HttpClientCache::default(),
            pg_distrib_dir: PathBuf::from("/pg"),
            yb_light_distrib_dir: PathBuf::from("/yb_light"),
            default_branch_id: None,
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
        let branch = BranchId::generate();
        let conf = allocate_safekeeper_conf(&env, "main", branch).unwrap();
        assert_eq!(conf.id, NodeId(1));
        assert_eq!(conf.pg_port, 5454);
        assert_eq!(conf.http_port, 7676);
        assert_eq!(conf.branch_id, Some(branch));
        assert_eq!(conf.endpoint_id, "main");
        assert_eq!(conf.yb_storage_conn.as_deref(), Some("{ host = 'h' }"));
    }

    #[test]
    fn second_safekeeper_gets_next_id_and_port_delta() {
        let mut env = env_with_templates();
        let main_branch = BranchId::generate();
        env.safekeepers
            .push(allocate_safekeeper_conf(&env.clone(), "main", main_branch).unwrap());
        let branch = BranchId::generate();
        let conf = allocate_safekeeper_conf(&env, "ep_b1", branch).unwrap();
        // id 2, delta = 2
        assert_eq!(conf.id, NodeId(2));
        assert_eq!(conf.pg_port, 5456);
        assert_eq!(conf.http_port, 7678);
        assert_eq!(conf.branch_id, Some(branch));
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
    fn pageserver_init_conf_preserves_template_yb_storage_conn() {
        let env = env_with_templates();
        let allocated = allocate_pageserver_conf(&env, "main").unwrap();
        let init_conf = pageserver_init_conf_for(&env, &allocated).unwrap();
        assert_eq!(init_conf.id, allocated.id);
        assert_eq!(init_conf.listen_pg_addr, allocated.listen_pg_addr);
        assert_eq!(init_conf.yb_storage_conn.as_deref(), Some("{ host = 'h' }"));
        assert!(init_conf.no_sync);
    }

    #[test]
    fn branch_drop_parses_branch_id() {
        let cli = Cli::try_parse_from([
            "yb_light_local",
            "branch",
            "drop",
            "--branch-id",
            "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0",
        ])
        .expect("`branch drop` with a branch id should parse");

        let YbLightLocalCmd::Branch(BranchCmd::Drop(args)) = cli.command else {
            panic!("expected `branch drop` to parse into Branch(Drop)");
        };
        let want_branch: BranchId = "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0".parse().unwrap();
        assert_eq!(args.branch_id, want_branch);
    }

    #[test]
    fn branch_drop_requires_branch_id() {
        // `--branch-id` is mandatory: dropping a branch must name the
        // exact branch, never fall back to a default.
        let res = Cli::try_parse_from(["yb_light_local", "branch", "drop"]);
        assert!(
            res.is_err(),
            "`branch drop` without --branch-id should fail to parse"
        );
    }

    #[test]
    fn branch_create_root_has_no_ancestor() {
        // `branch create --pg-version 18` with no ancestor is a ROOT branch.
        let cli = Cli::try_parse_from(["yb_light_local", "branch", "create", "--pg-version", "18"])
            .expect("`branch create --pg-version 18` should parse");

        let YbLightLocalCmd::Branch(BranchCmd::Create(args)) = cli.command else {
            panic!("expected `branch create` to parse into Branch(Create)");
        };
        assert!(
            args.ancestor_branch_name.is_none(),
            "root `branch create` should have no ancestor"
        );
    }

    #[test]
    fn branch_create_fork_resolves_ancestor() {
        // `branch create --branch-name <n> --ancestor-branch-name main
        // --branch-id <id>` is a FORK off the named ancestor.
        let cli = Cli::try_parse_from([
            "yb_light_local",
            "branch",
            "create",
            "--branch-name",
            "child",
            "--ancestor-branch-name",
            "main",
            "--branch-id",
            "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0",
        ])
        .expect("`branch create` fork form should parse");

        let YbLightLocalCmd::Branch(BranchCmd::Create(args)) = cli.command else {
            panic!("expected `branch create` to parse into Branch(Create)");
        };
        assert_eq!(args.ancestor_branch_name.as_deref(), Some("main"));
        assert_eq!(args.branch_name.as_deref(), Some("child"));
        let want_branch: BranchId = "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0".parse().unwrap();
        assert_eq!(args.branch_id, Some(want_branch));
    }
}
