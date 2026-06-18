// Command amp is the YB Light control plane (the "amp controller" /
// yblight-controller). See amp/architecture/yblc-highlvl.md and
// amp/architecture/yblight-controller.md.
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

	"github.com/yugabyte/yb-light/amp/internal/api"
	"github.com/yugabyte/yb-light/amp/internal/config"
	"github.com/yugabyte/yb-light/amp/internal/core"
	"github.com/yugabyte/yb-light/amp/internal/db"
	"github.com/yugabyte/yb-light/amp/internal/deploy"
	applog "github.com/yugabyte/yb-light/amp/internal/log"
	"github.com/yugabyte/yb-light/amp/internal/process"
	"github.com/yugabyte/yb-light/amp/internal/storagecontroller"
	"github.com/yugabyte/yb-light/amp/internal/task"
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
		fmt.Println("amp (dev)")
		return
	default:
		err = fmt.Errorf("unknown command %q (want: serve, migrate, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amp: "+err.Error())
		os.Exit(1)
	}
}

// buildProvisioning assembles the create-timeline / create-endpoint workflow's
// collaborators from config: the storage-controller client (when the managed
// controller is on) and the endpoint deployer (when --deploy is on). The deploy
// driver (--deploy-driver) selects the Docker or the Kubernetes/Helm Deployer;
// both consume the same EndpointSpec, so the create-endpoint workflow is
// identical. The SC URL it hands the deployer is amp's own network-reachable
// name + the controller's port, so a launched endpoint can register/attach back.
func buildProvisioning(cfg *config.Config, logger *slog.Logger) (api.Provisioning, error) {
	var prov api.Provisioning

	if cfg.StorageController.Enabled {
		prov.SC = storagecontroller.NewClient(cfg.StorageController.ListenAddr)
		// The URL a launched endpoint uses to reach the controller is amp's own
		// network-reachable name + the controller's port. If either can't be
		// resolved, leave it empty and warn — and fail below if it's actually needed.
		_, port, perr := net.SplitHostPort(cfg.StorageController.ListenAddr)
		host, herr := os.Hostname()
		switch {
		case perr != nil:
			logger.Warn("cannot parse storage controller listen address for the endpoint URL",
				"listen", cfg.StorageController.ListenAddr, "error", perr)
		case herr != nil || host == "":
			logger.Warn("cannot resolve hostname for the storage controller endpoint URL", "error", herr)
		default:
			prov.SCEndpointURL = fmt.Sprintf("http://%s:%s", host, port)
		}
	}

	if cfg.Deploy.Enabled {
		// Endpoints the deployer launches must be able to reach the managed
		// storage controller. If we couldn't resolve that URL, refuse to start
		// rather than silently launch controller-free endpoints while amp
		// registers their tenants with the controller (a split-brain). A config
		// override (--deploy-storage-controller-url) takes precedence when set
		// (e.g. an explicit in-cluster Service name for the Kubernetes driver).
		if cfg.Deploy.StorageControllerURL != "" {
			prov.SCEndpointURL = cfg.Deploy.StorageControllerURL
		}
		if cfg.StorageController.Enabled && prov.SCEndpointURL == "" {
			return api.Provisioning{}, fmt.Errorf(
				"endpoint deployment is enabled with the storage controller, but the controller URL endpoints should use could not be resolved (listen=%q); refusing to start",
				cfg.StorageController.ListenAddr)
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
				BackingYBExistingSecret:    cfg.Deploy.BackingYBExistingSecret,
				BackingYBExistingSecretKey: cfg.Deploy.BackingYBExistingSecretKey,
				Timeout:                    cfg.Deploy.Timeout,
			}, logger)
			logger.Info("kubernetes endpoint deployer enabled",
				"namespace", cfg.Deploy.Namespace,
				"chart", cfg.Deploy.ChartPath,
				"image", cfg.Deploy.EndpointImageRepo+":"+cfg.Deploy.EndpointImageTag,
				"backing_yb", cfg.Deploy.BackingYBHostPorts,
				"sc_endpoint_url", prov.SCEndpointURL)
		default: // config.DriverDocker
			prov.Deployer = deploy.NewDockerDeployer(deploy.DockerConfig{
				EndpointImage:  cfg.Deploy.EndpointImage,
				Network:        cfg.Deploy.Network,
				ExternalHost:   cfg.Deploy.ExternalHost,
				SupervisorPath: cfg.Deploy.SupervisorPath,
				ReadyTimeout:   cfg.Deploy.Timeout,
			}, logger)
			logger.Info("docker endpoint deployer enabled",
				"image", cfg.Deploy.EndpointImage, "network", cfg.Deploy.Network,
				"backing_yb", cfg.Deploy.BackingYBHostPorts,
				"sc_endpoint_url", prov.SCEndpointURL)
		}

		// Both drivers attach the launched compute to the backing YugabyteDB and
		// (when bootstrap is on) initdb the recorded tenant/timeline. This flows
		// into the EndpointSpec the handler builds for every endpoint.
		prov.EndpointYB = deploy.YBConn{
			HostPorts: cfg.Deploy.BackingYBHostPorts,
			User:      cfg.Deploy.BackingYBUser,
			Password:  cfg.Deploy.BackingYBPassword,
			DBName:    cfg.Deploy.BackingYBDBName,
		}
		prov.Bootstrap = cfg.Deploy.Bootstrap
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
	worker := task.NewWorker(store, task.DefaultRegistry(), cfg.Task, logger)
	backend := core.NewPgBackend(pool)

	// The create-timeline / create-endpoint workflow's collaborators: the storage
	// controller (to record the pending-init recipe) and the endpoint deployer
	// (to provision the compute, behind the configured driver).
	prov, err := buildProvisioning(cfg, logger)
	if err != nil {
		return err
	}
	srv := api.NewServer(cfg, backend, store, prov, logger)

	// Managed services the controller owns as subprocesses (§11). The storage
	// controller is the first; the manager is a no-op until one is enabled.
	mgr := process.NewManager(logger)
	if cfg.StorageController.Enabled {
		mgr.Add(storagecontroller.Spec(cfg.StorageController))
		logger.Info("storage controller managed as subprocess",
			"bin", cfg.StorageController.BinPath, "listen", cfg.StorageController.ListenAddr)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		mgr.Run(ctx)
	}()

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
	logger.Info("amp stopped")
	return nil
}
