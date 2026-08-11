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
	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
	applog "github.com/yugabyte/amp/controller/internal/log"
	"github.com/yugabyte/amp/controller/internal/metrics"
	"github.com/yugabyte/amp/controller/internal/node"
	"github.com/yugabyte/amp/controller/internal/registry"
	"github.com/yugabyte/amp/controller/internal/task"
	"github.com/yugabyte/amp/controller/internal/version"
	"github.com/yugabyte/amp/controller/internal/ycp"
)

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

// branchID is the 32-hex BranchId the controller maps a branch to (the storage
// layer is flat — the branch_id is the whole storage identity), and clusterID is
// the cluster whose branch table holds it.
func (a recipeStore) RecordBranchRecipe(ctx context.Context, clusterID uuid.UUID, branchID string, r task.BranchRecipe) error {
	var ancestor *string
	if r.AncestorBranchID != "" {
		ancestor = &r.AncestorBranchID
	}
	return a.s.RecordBranch(ctx, clusterID, branchID, registry.BranchCreateRecord{
		BranchName:       r.BranchName,
		PgVersion:        r.PgVersion,
		AncestorBranchID: ancestor,
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
				Namespace:                  cfg.Deploy.Namespace,
				ChartPath:                  cfg.Deploy.ChartPath,
				HelmBin:                    cfg.Deploy.HelmBin,
				KubeconfigPath:             cfg.Deploy.KubeconfigPath,
				EndpointImageRepo:          cfg.Deploy.EndpointImageRepo,
				EndpointImageTag:           cfg.Deploy.EndpointImageTag,
				PullSecret:                 cfg.Deploy.PullSecret,
				ComputePort:                cfg.Deploy.ComputePort,
				ServiceType:                cfg.Deploy.ServiceType,
				ServiceAnnotations:         cfg.Deploy.ServiceAnnotations,
				NodeSelector:               cfg.Deploy.NodeSelector,
				TailnetSuffix:              cfg.Deploy.TailnetSuffix,
				StorageClass:               cfg.Deploy.StorageClass,
				DiskSize:                   cfg.Deploy.DiskSize,
				DiskGiBPerVCPU:             cfg.Deploy.DiskGiBPerVCPU,
				BackingYBExistingSecret:    cfg.Deploy.BackingYBExistingSecret,
				BackingYBExistingSecretKey: cfg.Deploy.BackingYBExistingSecretKey,
				Timeout:                    cfg.Deploy.Timeout,
			}, logger)
			// Externalized extensions are mounted as ImageVolumes, which only the
			// Kubernetes runtime supports; the catalog resolves a branch's set to
			// pinned images for the endpoint spec (compute/architecture/extensions.md).
			prov.Extensions = extensions.NewCatalog(cfg.Deploy.ExtensionRegistry)
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
	deps := task.Deps{
		State:         backend,
		Deployer:      prov.Deployer,
		EndpointYB:    prov.EndpointYB,
		ControllerURL: prov.ControllerURL,
		Recipes:       prov.Recipes,
		Extensions:    prov.Extensions,
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

	worker := task.NewWorker(store, task.ProductionRegistry(deps), cfg.Task, logger, task.WithMetrics(metricsReg))

	// The API creates tasks and reads their status through the task service (which
	// wraps the same store the worker drains).
	srv := api.NewServer(cfg, backend, task.NewService(store), prov, metricsReg, logger)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Run(ctx)
	}()

	// YCP fleet registration + reporting (opt-in via --ycp). The controller
	// enrolls itself as an "amp" environment and reports the storage pools /
	// databases / branches / endpoints it owns on a heartbeat, reading them from
	// the same backend the API serves. It reconciles no desired state yet.
	if cfg.YCP.Enabled {
		ycpLoop := ycp.NewLoop(
			cfg.YCP,
			ycp.NewClient(cfg.YCP.URL, nil),
			ycp.NewStore(pool.Pool),
			backend,
			ycp.ClusterFacts{},
			version.Get().Version,
			logger,
		)
		logger.Info("ycp reporting enabled", "url", cfg.YCP.URL, "env_name", cfg.YCP.EnvName, "interval", cfg.YCP.ReportInterval)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ycpLoop.Run(ctx)
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
