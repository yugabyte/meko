// Command controller is the YB Amp control plane (the "amp controller" /
// amp-controller). See controller/architecture/ac-highlvl.md and
// controller/architecture/amp-controller.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/branchgc"
	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
	"github.com/yugabyte/amp/controller/internal/gc"
	"github.com/yugabyte/amp/controller/internal/ingress"
	applog "github.com/yugabyte/amp/controller/internal/log"
	"github.com/yugabyte/amp/controller/internal/metrics"
	"github.com/yugabyte/amp/controller/internal/node"
	"github.com/yugabyte/amp/controller/internal/registry"
	"github.com/yugabyte/amp/controller/internal/task"
	"github.com/yugabyte/amp/controller/internal/version"
	"github.com/yugabyte/amp/controller/internal/voyager/migration"
	voyagermigrations "github.com/yugabyte/amp/controller/internal/voyager/migrations"
	"github.com/yugabyte/amp/controller/internal/ycp"
)

// The kubernetes deployer is what reads a shard's load-balancer address. Asserted
// at compile time as well as at the type switch below, so a change to either side
// of the interface fails the build rather than turning into a startup error on
// somebody's cluster.
var _ ingress.AddressLookup = (*deploy.KubernetesDeployer)(nil)

// ingressReconcileInterval is how often a building ingress shard is re-checked
// for its load-balancer address. Cloud provisioning takes minutes, so this is
// paced for "not yet" being the common answer rather than for latency: nothing
// is waiting on a shard at startup, and the chart that installs the first
// shard's release lands later in this series.
const ingressReconcileInterval = 15 * time.Second

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "migrate":
		err = runMigrate(args)
	case "version":
		fmt.Println("controller (dev)")
		return
	default:
		err = fmt.Errorf("unknown command %q (want: serve, migrate, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "controller: "+err.Error())
		os.Exit(1)
	}
}

// recipeStore adapts the in-process storage-layer store to the task framework's
// task.RecipeStore port, converting the framework's decoupled BranchRecipe into
// a branch creation record. It keeps the task package independent of the store type.
type recipeStore struct{ s *registry.Store }

// branchID is the whole identity, the storage layer being flat (no enclosing
// tenant). clusterID is the cluster whose branch table holds it.
func (a recipeStore) RecordBranchRecipe(ctx context.Context, clusterID, branchID uuid.UUID, r task.BranchRecipe) error {
	return a.s.RecordBranch(ctx, clusterID, branchID, registry.BranchCreateRecord{
		BranchName:       r.BranchName,
		PgVersion:        r.PgVersion,
		AncestorBranchID: r.AncestorBranchID,
		StartLSN:         r.StartLSN,
		PgSettings:       r.PgSettings,
	})
}

// buildProvisioning assembles the create-branch / create-endpoint workflow's
// collaborators from config: the endpoint deployer (when --deploy is on) and the
// URL launched endpoints use to reach the controller's storage-layer surface. The
// deploy driver (--deploy-driver) selects the Docker or the Kubernetes/Helm
// Deployer; both consume the same EndpointSpec, so the create-endpoint workflow is
// identical.
func buildProvisioning(cfg *config.Config, logger *slog.Logger) (api.Provisioning, error) {
	var prov api.Provisioning
	// Set whatever the driver: it shapes what the API accepts, not what any one
	// deployer launches.
	prov.MemoryGiBPerVCPU = cfg.Deploy.MemoryGiBPerVCPU

	if cfg.Deploy.Enabled {
		// Records only: the compute this controller names is deployed by someone
		// else (yb_amp_local spawns its own), so there is no image to launch, no
		// controller URL for it to dial back on, and no safekeeper the controller
		// could reach — prov.Node stays nil, which is what makes a fork without an
		// explicit fork point a clean 400 rather than a failed HTTP call.
		if cfg.Deploy.Driver == config.DriverLocal {
			prov.Deployer = deploy.LocalDeployer{}
			logger.Info("local endpoint deployer enabled; endpoints are records only, nothing is launched")
			return prov, nil
		}

		// Launched endpoints reach the controller's storage-layer surface at the
		// controller's own network-reachable URL. An explicit override wins (e.g.
		// an in-cluster Service URL for the Kubernetes driver); otherwise derive it
		// from the controller's hostname + listen port. If neither resolves, refuse
		// to start rather than launch endpoints that can't reach back.
		if cfg.Deploy.ControllerURL != "" {
			prov.ControllerURL = cfg.Deploy.ControllerURL
		} else {
			_, port, perr := net.SplitHostPort(cfg.ListenAddr)
			host, herr := os.Hostname()
			switch {
			case perr != nil:
				logger.Warn("cannot parse controller listen address for the endpoint URL",
					"listen", cfg.ListenAddr, "error", perr)
			case herr != nil || host == "":
				logger.Warn("cannot resolve hostname for the endpoint URL", "error", herr)
			default:
				prov.ControllerURL = fmt.Sprintf("http://%s:%s", host, port)
			}
		}
		if prov.ControllerURL == "" {
			return api.Provisioning{}, fmt.Errorf(
				"endpoint deployment is enabled but the controller URL endpoints should use could not be resolved (listen=%q); refusing to start",
				cfg.ListenAddr)
		}

		switch cfg.Deploy.Driver {
		case config.DriverKubernetes:
			prov.Deployer = deploy.NewKubernetesDeployer(deploy.KubernetesConfig{
				Namespace:                     cfg.Deploy.Namespace,
				ChartPath:                     cfg.Deploy.ChartPath,
				HelmBin:                       cfg.Deploy.HelmBin,
				KubeconfigPath:                cfg.Deploy.KubeconfigPath,
				EndpointImageRepo:             cfg.Deploy.EndpointImageRepo,
				EndpointImageTag:              cfg.Deploy.EndpointImageTag,
				DocumentDBGatewayImageRepo:    cfg.Deploy.DocumentDBGatewayImageRepo,
				DocumentDBGatewayImageTag:     cfg.Deploy.DocumentDBGatewayImageTag,
				PullSecret:                    cfg.Deploy.PullSecret,
				ComputePort:                   cfg.Deploy.ComputePort,
				ServiceType:                   cfg.Deploy.ServiceType,
				ServiceAnnotations:            cfg.Deploy.ServiceAnnotations,
				Cloud:                         cfg.Deploy.Cloud,
				IngressEnabled:                cfg.Deploy.IngressEnabled,
				NodeSelector:                  cfg.Deploy.NodeSelector,
				TailnetSuffix:                 cfg.Deploy.TailnetSuffix,
				StorageClass:                  cfg.Deploy.StorageClass,
				DiskSize:                      cfg.Deploy.DiskSize,
				DiskGiBPerVCPU:                cfg.Deploy.DiskGiBPerVCPU,
				DiskSizeOverride:              cfg.Deploy.DiskSizeOverride,
				LFCOffloadIntervalSeconds:     cfg.Deploy.LFCOffloadIntervalSeconds,
				BackingYBExistingSecret:       cfg.Deploy.BackingYBExistingSecret,
				BackingYBExistingSecretKey:    cfg.Deploy.BackingYBExistingSecretKey,
				BackingYBTLSMode:              cfg.Deploy.BackingYBTLSMode,
				BackingYBTLSIssuerKind:        cfg.Deploy.BackingYBTLSIssuerKind,
				BackingYBTLSIssuerName:        cfg.Deploy.BackingYBTLSIssuerName,
				BackingYBTLSIssuerGroup:       cfg.Deploy.BackingYBTLSIssuerGroup,
				BackingYBTLSCABundleConfigMap: cfg.Deploy.BackingYBTLSCABundleConfigMap,
				BackingYBTLSCertDuration:      cfg.Deploy.BackingYBTLSCertDuration,
				BackingYBTLSCertRenewBefore:   cfg.Deploy.BackingYBTLSCertRenewBefore,
				ComputeTLSMode:                cfg.Deploy.ComputeTLSMode,
				ComputeTLSIssuerKind:          cfg.Deploy.ComputeTLSIssuerKind,
				ComputeTLSIssuerName:          cfg.Deploy.ComputeTLSIssuerName,
				ComputeTLSIssuerGroup:         cfg.Deploy.ComputeTLSIssuerGroup,
				ComputeTLSSecret:              cfg.Deploy.ComputeTLSSecret,
				ComputeTLSCertDuration:        cfg.Deploy.ComputeTLSCertDuration,
				ComputeTLSCertRenewBefore:     cfg.Deploy.ComputeTLSCertRenewBefore,
				ComputeTLSIncludeServiceDNS:   cfg.Deploy.ComputeTLSIncludeServiceDNS,
				Timeout:                       cfg.Deploy.Timeout,
			}, logger)
			prov.EndpointVersion = cfg.Deploy.EndpointImageTag
			// Externalized extensions are mounted as ImageVolumes, which only the
			// Kubernetes runtime supports; the service resolves a branch's set to
			// pinned images for the endpoint spec (compute/architecture/extensions.md).
			// Built-ins only here — buildProvisioning runs before the database
			// backend exists, so runServe rebinds this with the
			// registered-extension store as soon as it does.
			prov.Extensions = extensions.NewService(
				extensions.NewCatalog(cfg.Deploy.ExtensionRegistry), nil)
			logger.Info("kubernetes endpoint deployer enabled",
				"namespace", cfg.Deploy.Namespace,
				"chart", cfg.Deploy.ChartPath,
				"image", cfg.Deploy.EndpointImageRepo+":"+cfg.Deploy.EndpointImageTag,
				"extension_registry", cfg.Deploy.ExtensionRegistry,
				"backing_yb", cfg.Deploy.BackingYBHostPorts,
				"storage_class", cfg.Deploy.StorageClass,
				"controller_url", prov.ControllerURL)
		default: // config.DriverDocker
			prov.Deployer = deploy.NewDockerDeployer(deploy.DockerConfig{
				EndpointImage: cfg.Deploy.EndpointImage,
				Network:       cfg.Deploy.Network,
				ExternalHost:  cfg.Deploy.ExternalHost,
				ReadyTimeout:  cfg.Deploy.Timeout,
			}, logger)
			// The tag of the one configured reference, where Kubernetes configures a
			// repo and a tag apart. Empty for a digest-pinned image, which names no
			// version — the upgrade route refuses rather than inventing one.
			prov.EndpointVersion = deploy.ImageRefTag(cfg.Deploy.EndpointImage)
			logger.Info("docker endpoint deployer enabled",
				"image", cfg.Deploy.EndpointImage, "network", cfg.Deploy.Network,
				"backing_yb", cfg.Deploy.BackingYBHostPorts,
				"controller_url", prov.ControllerURL)
		}

		// The node client reads a branch's default fork point from the parent's own
		// safekeeper (addressed via the deployer); it is only useful with a deployer,
		// so it is wired here alongside one (#133).
		prov.Node = node.NewClient()

		// Both drivers attach the launched compute to the backing YugabyteDB and
		// (when bootstrap is on) initdb the recorded tenant/timeline. This flows
		// into the EndpointSpec the handler builds for every endpoint.
		prov.EndpointYB = deploy.YBConn{
			HostPorts: cfg.Deploy.BackingYBHostPorts,
			User:      cfg.Deploy.BackingYBUser,
			Password:  cfg.Deploy.BackingYBPassword,
			DBName:    cfg.Deploy.BackingYBDBName,
		}
	}

	// The Voyager migration job runner launches offline-migration jobs through the
	// same driver the endpoint deployer uses. It is wired independently of
	// cfg.Deploy.Enabled — the migration feature may be on while endpoint
	// deployment is off — but config validation has already restricted the driver
	// to docker or kubernetes when the feature is enabled.
	if cfg.Voyager.DatabaseURL != "" {
		switch cfg.Deploy.Driver {
		case config.DriverKubernetes:
			prov.VoyagerRunner = deploy.NewKubernetesVoyagerJobRunner(deploy.KubernetesVoyagerConfig{
				Image:              cfg.Voyager.Image,
				Namespace:          cfg.Voyager.Namespace,
				PVCSize:            cfg.Voyager.PVCSize,
				StorageClass:       cfg.Voyager.StorageClass,
				PullSecret:         cfg.Deploy.PullSecret,
				DBConnectionString: cfg.Voyager.DatabaseURL,
			})
		default: // config.DriverDocker
			prov.VoyagerRunner = deploy.NewDockerVoyagerJobRunner(deploy.DockerVoyagerConfig{
				Image:              cfg.Voyager.Image,
				Network:            cfg.Deploy.Network,
				DBConnectionString: cfg.Voyager.DatabaseURL,
			})
		}
		logger.Info("Voyager migration job runner enabled",
			"driver", cfg.Deploy.Driver, "image", cfg.Voyager.Image)
	}
	return prov, nil
}

// runMigrate applies migrations and exits — useful as an init container or a
// pre-deploy step.
func runMigrate(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	logger := applog.New(cfg.Log)
	slog.SetDefault(logger)

	ver := version.Get()
	logger.Info("amp controller starting", "version", ver.Version, "commit", ver.Commit, "build_date", ver.BuildDate)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, cfg.DB.URL, cfg.DB.Schema); err != nil {
		return err
	}
	logger.Info("migrations applied", "schema", cfg.DB.Schema)

	// Apply the Voyager control schema too when the feature is configured, so a
	// migrate init-container prepares both databases in one pass.
	if cfg.Voyager.DatabaseURL != "" {
		if err := db.MigrateFS(ctx, cfg.Voyager.DatabaseURL, voyagermigrations.ControlSchema, voyagermigrations.FS); err != nil {
			return fmt.Errorf("migrate Voyager state: %w", err)
		}
		logger.Info("Voyager migrations applied", "schema", voyagermigrations.ControlSchema)
	}
	return nil
}

func runServe(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	logger := applog.New(cfg.Log)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DB.URL, cfg.DB.Schema)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, cfg.DB.URL, cfg.DB.Schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	logger.Info("migrations applied", "schema", cfg.DB.Schema)

	// The Voyager migration feature keeps its control tables in a schema (and,
	// typically, a database) of their own. When configured, apply that schema and
	// open a second pool pinned to it; when not, voyagerPool stays nil and the
	// migration APIs answer 501, leaving the controller's behavior unchanged.
	var voyagerPool *db.Pool
	if cfg.Voyager.DatabaseURL != "" {
		if err := db.MigrateFS(ctx, cfg.Voyager.DatabaseURL, voyagermigrations.ControlSchema, voyagermigrations.FS); err != nil {
			return fmt.Errorf("migrate Voyager state: %w", err)
		}
		logger.Info("Voyager migrations applied", "schema", voyagermigrations.ControlSchema)
		voyagerPool, err = db.Connect(ctx, cfg.Voyager.DatabaseURL, voyagermigrations.ControlSchema)
		if err != nil {
			return fmt.Errorf("connect to Voyager database: %w", err)
		}
		defer voyagerPool.Close()
	}

	store := task.NewPgStore(pool)
	backend := core.NewPgBackend(pool)
	regStore := registry.NewStore(pool)

	// The create-timeline / create-endpoint workflow's collaborators: the storage
	// controller (to record the pending-init recipe) and the endpoint deployer
	// (to provision the compute, behind the configured driver).
	prov, err := buildProvisioning(cfg, logger)
	if err != nil {
		return err
	}

	// Wire the write-ahead task framework's step catalog against the same
	// collaborators the API handlers' cheap pre-checks use. The mutation handlers
	// plan tasks through the Service (#113), so the worker drains every
	// createCluster/createBranch/createEndpoint/deleteEndpoint/deleteBranch/
	// deleteCluster. *core.PgBackend supplies the idempotent, id-keyed state
	// operations the steps need.
	prov.Recipes = recipeStore{s: regStore}

	// Not fatal: an unseeded fleet version only means the upgrade route cannot
	// tell a converged fleet from one that is not, and so does not refuse a roll.
	// A controller with no endpoint version has no fleet version to seed.
	if prov.EndpointVersion != "" {
		if err := core.SeedFleetVersion(ctx, backend, prov.EndpointVersion); err != nil {
			logger.Error("seed fleet version", "error", err)
		}
	}

	// Now that the backend exists, give the extension service its store: the
	// built-in catalog buildProvisioning made, plus the extensions an operator
	// registered (amp_registry.extensions). Only when externalized extensions are
	// enabled at all — a nil service means the Docker driver, which has no
	// ImageVolumes to mount.
	if prov.Extensions != nil {
		prov.Extensions = extensions.NewService(prov.Extensions.Builtin(), backend)
	}

	// Wire the migration store + derive-on-read service onto the provisioning the
	// API handlers read (nil-safe: they stay nil when the feature is off, which is
	// what makes the migration handlers answer 501).
	if cfg.Voyager.DatabaseURL != "" {
		prov.VoyagerMigrations = migration.NewYbStore(voyagerPool)
		prov.VoyagerMigrationService = migration.NewService(prov.VoyagerMigrations, prov.VoyagerRunner)
	}

	// Shared ingress: nil unless this deployment opted in with
	// --deploy-ingress-enabled, not on whether a zone is set — the sibling block
	// below is keyed the same way.
	//
	// What makes the ingress_* modes refusable at the API is the DEPLOYER
	// (SupportsIngress, and PlacesIngress once a route can be rendered), read in
	// handlers.go; a nil here only means the steps have no allocator to reach,
	// which is why each one checks (controller/architecture/ac-ingress.md).
	//
	// The zones are separate from the switch: a kind or CI cluster routes through
	// shards and has no DNS at all, and then names are allocated but unpublished.
	// One per family so external-dns can be pinned to each and an internal
	// endpoint's name is never published to a public zone.
	ingressZones := ingress.Zones{
		Internal: cfg.Deploy.IngressZoneInternal,
		External: cfg.Deploy.IngressZoneExternal,
	}
	var ingressAlloc *ingress.Allocator
	if cfg.Deploy.IngressEnabled {
		ingressAlloc = ingress.New(pool, true, ingressZones, cfg.Deploy.IngressQuarantine,
			ingress.Placement(cfg.Deploy.IngressPlacement))
	}

	deps := task.Deps{
		State:         backend,
		Tasks:         store,
		Deployer:      prov.Deployer,
		EndpointYB:    prov.EndpointYB,
		ControllerURL: prov.ControllerURL,
		Recipes:       prov.Recipes,
		Extensions:    prov.Extensions,
		// Voyager-migration ports (nil when the feature is off). The endpoint
		// lookup adapter keeps the task package off the core domain layer.
		VoyagerMigrations:         prov.VoyagerMigrations,
		VoyagerMigrationEndpoints: migrationEndpointLookup{backend: backend},
		VoyagerRunner:             prov.VoyagerRunner,
		// The nlb_* modes publish names in the same zones as the ingress_* ones,
		// whether or not the shared ingress is on.
		DNSZones: ingressZones,
	}
	// Typed nil in an interface is not nil, so only assign when there is one —
	// otherwise the steps' `if s.ingress != nil` guard would pass and dereference.
	if ingressAlloc != nil {
		deps.Ingress = ingressAlloc
	}
	// Prometheus instrumentation: one registry, shared by the worker (task
	// metrics) and the HTTP server (request + runtime metrics, and the /metrics
	// scrape endpoint). Step 1 of the amp metrics pipeline
	// (architecture/ac-metrics.md). Nil when --metrics=false, which every consumer
	// treats as a no-op (the scrape endpoint is simply not mounted).
	var metricsReg *metrics.Metrics
	if cfg.Metrics {
		metricsReg = metrics.New()
		// Export the registry inventory gauges (branches / pageserver nodes),
		// counted from the registry store on each scrape.
		metricsReg.WithRegistrySource(regStore, logger)
	} else {
		logger.Info("metrics disabled (--metrics=false); /metrics not served")
	}

	// The task and endpoint leases (architecture/ac-locking.md), over the same pool
	// as the task log. Losing one exits the process, so a failure to build the
	// client is fatal here rather than degrading to an unlocked worker.
	locks, err := task.NewPgLocks(pool, logger, cfg.Task.LeaseTTL, cfg.Task.RenewInterval, cfg.Task.LockWait)
	if err != nil {
		return fmt.Errorf("task locks: %w", err)
	}
	subtasks := task.NewPgSubtasks(pool)
	worker := task.NewWorker(store, subtasks, task.ProductionRegistry(deps), locks, cfg.Task, logger,
		task.WithMetrics(metricsReg))

	// One service over the same store the worker drains, for everything that
	// initiates or reads tasks: the API, and the GC sweeper below.
	tasks := task.NewService(store, subtasks)
	srv := api.NewServer(cfg, backend, tasks, prov, metricsReg, logger)
	// The write-once YCP binding endpoint the reconcile-agent pins itself to.
	srv.RegisterYCPBinding(ycp.NewBindingStore(pool.Pool))

	// Reclaims clusters and endpoints whose create failed, by enqueueing the same
	// delete task an operator's delete would. Runs beside the worker rather than
	// inside it: it produces tasks, it does not execute them.
	sweeper := gc.New(backend, tasks, gc.Config{
		Enabled:    cfg.GC.Enabled,
		Interval:   cfg.GC.Interval,
		Grace:      cfg.GC.Grace,
		BatchLimit: cfg.GC.BatchLimit,
	}, logger, metricsReg)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		worker.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		sweeper.Run(ctx)
	}()

	// The branch-drop reaper: reclaim a dropped branch's amp_storage rows once its
	// retention window has passed. A goroutine of its own beside the worker, on the
	// same shutdown context — the task framework has neither retries nor a
	// run_after column, so periodic work cannot be a task, and every replica
	// running one is safe (the deletes are idempotent per branch).
	reaper := branchgc.New(pool, cfg.BranchGC, logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		reaper.Run(ctx)
	}()

	// Shared ingress: register the first shard of each configured family and wait
	// for the cloud to give its load balancer an address. Only then is it
	// allocatable — an endpoint placed on a shard with no address would publish a
	// DNS record pointing nowhere (controller/architecture/ac-ingress.md).
	//
	// Keyed on the switch, not on the zones. Zones set with the switch off is a
	// valid configuration — validateIngress checks the knobs whether or not
	// ingress is on, precisely so they can sit there until somebody flips it —
	// and registering a shard for such a deployment would poll a Service nothing
	// installed, every 15s, forever.
	//
	// Both steps are idempotent, because a controller restart is the ordinary
	// case: EnsureShard writes nothing onto an existing row and MarkReady only
	// promotes a shard still building.
	lookup, canLookUp := prov.Deployer.(ingress.AddressLookup)
	switch {
	case ingressAlloc == nil:
		// Off, which is every deployment today.
	case !canLookUp:
		// A config the API already handles per request with a 400, so refusing to
		// start would be a harsher answer to the same mistake — and with --deploy
		// off there is no deployer at all. Skipped and said out loud instead.
		logger.Warn("shared ingress is enabled but this deploy driver cannot address a shard; "+
			"no shard will be registered and the ingress_* modes stay refused",
			"driver", cfg.Deploy.Driver)
	default:
		alloc := ingressAlloc
		// One shard per family the deployment actually serves. A shard is a cloud
		// load balancer, so this follows stated intent rather than registering
		// both: the zones are that intent, and registering the internal family
		// unconditionally would leave an external-only deployment holding a shard
		// it does not want and none for the family it does.
		//
		// With no zone at all — a kind or CI cluster, where endpoints are
		// addressed by their shard — there is no intent to read, so the internal
		// family is the default. An external shard on such a cluster would be an
		// internet-facing load balancer nobody asked for.
		families := []ingress.Family{}
		for _, f := range []ingress.Family{ingress.FamilyInternal, ingress.FamilyExternal} {
			if ingressZones.For(f) != "" {
				families = append(families, f)
			}
		}
		if len(families) == 0 {
			families = []ingress.Family{ingress.FamilyInternal}
		}
		for _, f := range families {
			if err := alloc.EnsureShard(ctx, ingress.Shard{
				Name:   ingress.FirstShardNameFor(f),
				Family: f,
				PortLo: cfg.Deploy.IngressPortLo,
				PortHi: cfg.Deploy.IngressPortHi,
			}); err != nil {
				stop()
				wg.Wait()
				return err
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(ingressReconcileInterval)
			defer ticker.Stop()
			for {
				if err := alloc.ReconcileShards(ctx, lookup, logger); err != nil {
					logger.Warn("ingress shard reconcile", "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}

	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Start() }()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	case err := <-srvErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			stop()
			wg.Wait()
			return fmt.Errorf("server: %w", err)
		}
	}

	stop() // cancel ctx so the worker drains
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown error", "error", err)
	}
	wg.Wait()
	logger.Info("controller stopped")
	return nil
}
