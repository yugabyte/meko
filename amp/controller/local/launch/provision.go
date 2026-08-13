package launch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yugabyte/amp/controller/local/cfg"
	"github.com/yugabyte/amp/controller/local/client"
	"github.com/yugabyte/amp/controller/local/config"
)

// firstPageserverTemplate returns the sole pageserver template (the launcher
// configures exactly one).
func firstPageserverTemplate(env *config.LocalEnv) (*config.YbAmpLocalInitPageserverConf, error) {
	if len(env.PageserverTemplates) == 0 {
		return nil, fmt.Errorf("no pageserver template configured")
	}
	return &env.PageserverTemplates[0], nil
}

// firstSafekeeperTemplate returns the sole safekeeper template.
func firstSafekeeperTemplate(env *config.LocalEnv) (*config.SafekeeperConf, error) {
	if len(env.SafekeeperTemplates) == 0 {
		return nil, fmt.Errorf("no safekeeper template configured")
	}
	return &env.SafekeeperTemplates[0], nil
}

// allocatePageserverConf allocates the conf for a pageserver provisioned under
// endpointID: the template ports bumped by the lowest free even delta (0 when
// none are provisioned, so the first pageserver reuses the template ports;
// freed deltas are reused after deprovisioning). Mirrors Rust
// `allocate_pageserver_conf`.
func allocatePageserverConf(env *config.LocalEnv, endpointID string) (config.PageServerConf, error) {
	template, err := firstPageserverTemplate(env)
	if err != nil {
		return config.PageServerConf{}, err
	}
	basePg, err := portOf(template.ListenPgAddr)
	if err != nil {
		return config.PageServerConf{}, err
	}
	used := map[uint16]bool{}
	for _, p := range env.Pageservers {
		port, err := portOf(p.ListenPgAddr)
		if err != nil {
			return config.PageServerConf{}, err
		}
		used[port] = true
	}
	var delta uint16
	for used[basePg+delta] {
		delta += 2
	}

	pgAddr, err := bumpAddrPort(template.ListenPgAddr, delta)
	if err != nil {
		return config.PageServerConf{}, err
	}
	httpAddr, err := bumpAddrPort(template.ListenHTTPAddr, delta)
	if err != nil {
		return config.PageServerConf{}, err
	}
	conf := config.PageServerConf{
		ListenPgAddr:   pgAddr,
		ListenHTTPAddr: httpAddr,
		PgAuthType:     template.PgAuthType,
		HTTPAuthType:   template.HTTPAuthType,
		GRPCAuthType:   template.GRPCAuthType,
		NoSync:         template.NoSync,
		DataDir:        env.PageserverDataDir(endpointID),
	}
	if template.ListenHTTPSAddr != nil {
		a, err := bumpAddrPort(*template.ListenHTTPSAddr, delta)
		if err != nil {
			return config.PageServerConf{}, err
		}
		conf.ListenHTTPSAddr = &a
	}
	if template.ListenGRPCAddr != nil {
		a, err := bumpAddrPort(*template.ListenGRPCAddr, delta)
		if err != nil {
			return config.PageServerConf{}, err
		}
		conf.ListenGRPCAddr = &a
	}
	return conf, nil
}

// pageserverInitConfFor builds the full init conf (carrying yb_storage_conn +
// passthrough keys) for a pageserver, by cloning the lowest-id template and
// stamping in the allocated id/ports/auth. Mirrors Rust
// `pageserver_init_conf_for`.
func pageserverInitConfFor(env *config.LocalEnv, allocated *config.PageServerConf) (config.YbAmpLocalInitPageserverConf, error) {
	template, err := firstPageserverTemplate(env)
	if err != nil {
		return config.YbAmpLocalInitPageserverConf{}, err
	}
	conf := config.YbAmpLocalInitPageserverConf{
		ListenPgAddr:    allocated.ListenPgAddr,
		ListenHTTPAddr:  allocated.ListenHTTPAddr,
		ListenHTTPSAddr: allocated.ListenHTTPSAddr,
		ListenGRPCAddr:  allocated.ListenGRPCAddr,
		PgAuthType:      allocated.PgAuthType,
		HTTPAuthType:    allocated.HTTPAuthType,
		GRPCAuthType:    allocated.GRPCAuthType,
		NoSync:          allocated.NoSync,
		YbStorageConn:   template.YbStorageConn,
		Other:           template.Other,
	}
	return conf, nil
}

// allocateSafekeeperConf allocates the conf for a safekeeper provisioned under
// endpointID, dedicated to branchID. Mirrors Rust `allocate_safekeeper_conf`.
func allocateSafekeeperConf(env *config.LocalEnv, endpointID string, branchID config.BranchID) (config.SafekeeperConf, error) {
	template, err := firstSafekeeperTemplate(env)
	if err != nil {
		return config.SafekeeperConf{}, err
	}
	used := map[uint16]bool{}
	for _, s := range env.Safekeepers {
		used[s.PgPort] = true
	}
	var delta uint16
	for used[template.PgPort+delta] {
		delta += 2
	}

	bid := branchID
	conf := config.SafekeeperConf{
		PgPort:          template.PgPort + delta,
		HTTPPort:        template.HTTPPort + delta,
		BackupThreads:   template.BackupThreads,
		AuthEnabled:     template.AuthEnabled,
		ListenAddr:      template.ListenAddr,
		YbStorageConn:   template.YbStorageConn,
		WalCacheSizeMib: template.WalCacheSizeMib,
		BranchID:        &bid,
		EndpointID:      endpointID,
	}
	if template.PgTenantOnlyPort != nil {
		p := *template.PgTenantOnlyPort + delta
		conf.PgTenantOnlyPort = &p
	}
	if template.HTTPSPort != nil {
		p := *template.HTTPSPort + delta
		conf.HTTPSPort = &p
	}
	return conf, nil
}

// ancestorDurableLSN reads the ancestor branch's durable WAL end (flush_lsn)
// from its safekeeper — the authoritative branch point. Mirrors Rust
// `ancestor_durable_lsn`.
func ancestorDurableLSN(ctx context.Context, env *config.LocalEnv, branchID config.BranchID) (config.Lsn, error) {
	sks := safekeepersForTimeline(env, branchID)
	if len(sks) == 0 {
		return "", fmt.Errorf("no safekeeper serves branch %s", branchID)
	}
	sk := sks[0]
	listenAddr := "127.0.0.1"
	if sk.ListenAddr != nil {
		listenAddr = *sk.ListenAddr
	}
	c := client.NewSafekeeper(fmt.Sprintf("%s:%d", listenAddr, sk.HTTPPort), httpClient())
	status, err := c.TimelineStatus(ctx, branchID)
	if err != nil {
		return "", fmt.Errorf("fetch safekeeper branch status: %w", err)
	}
	return status.FlushLSN, nil
}

// Creating the branch on the safekeeper, and waiting for the pageserver to report
// it Active, are the supervisor's: it does both between starting the pageserver
// and starting the compute, which is where they have to happen. See
// compute/endpoint_supervisor/src/coordinate.rs.

// provisionTimelineForEndpoint provisions the pageserver + safekeeper pair
// serving branchID, co-located under endpointID, from the branch's creation
// record. No-op when a pageserver already serves the branch. Mirrors Rust
// `provision_timeline_for_endpoint`.
func provisionTimelineForEndpoint(ctx context.Context, env *config.LocalEnv, endpointID string, branchID config.BranchID) error {
	if env.PageserverConfForBranch(branchID) != nil {
		return nil
	}
	b, err := newBranchAPI(env)
	if err != nil {
		return err
	}
	branch, err := b.resolveBranch(ctx, branchID)
	if err != nil {
		return err
	}
	if branch == nil {
		return fmt.Errorf(
			"branch %s has no provisioned pageserver and no creation record; run `branch create` first", branchID)
	}

	// A NULL parent means bootstrap (initdb + import); a set one means fork that
	// ancestor at its fork point.
	if branch.ParentBranchId == nil {
		return provisionBootstrap(ctx, env, endpointID, branchID, branch.PgVersion)
	}
	forkLSN := ""
	if branch.ForkLsn != nil {
		forkLSN = *branch.ForkLsn
	}
	return provisionBranch(ctx, env, endpointID, branchID,
		config.BranchID(hex32UUID(*branch.ParentBranchId)), forkLSN, branch.PgVersion)
}

// provisionBootstrap lays out the main branch's pair: allocate their ports,
// write their config, and bake the pageserver's branch identity. Mirrors Rust
// `provision_bootstrap`.
//
// Nothing is started here. `endpoint start` runs the supervisor, which starts the
// safekeeper, then the pageserver (which runs initdb + import), then waits for
// the branch to go Active and creates the timeline on the safekeeper at the LSN
// it reports.
func provisionBootstrap(_ context.Context, env *config.LocalEnv, endpointID string, branchID config.BranchID, _ int) error {
	psConf, err := allocatePageserverConf(env, endpointID)
	if err != nil {
		return err
	}
	skConf, err := allocateSafekeeperConf(env, endpointID, branchID)
	if err != nil {
		return err
	}

	fmt.Printf("Provisioning safekeeper (pg port %d) for endpoint '%s'\n", skConf.PgPort, endpointID)
	if err := writeSafekeeperFiles(env, &skConf); err != nil {
		return err
	}
	env.Safekeepers = append(env.Safekeepers, skConf)

	fmt.Printf("Provisioning pageserver (pg %s) for endpoint '%s'\n", psConf.ListenPgAddr, endpointID)
	if err := os.MkdirAll(psConf.DataDir, 0o755); err != nil {
		return err
	}
	if err := writePageserverFiles(env, &psConf, &skConf); err != nil {
		return err
	}
	// Bake the identity. How to create the timeline comes from the
	// controller-seeded branch_state row, not from here.
	branchIDCopy := branchID
	psConf.BranchID = &branchIDCopy
	if err := cfg.SetPageserverIdentity(filepath.Join(psConf.DataDir, "pageserver.toml"), branchID); err != nil {
		return err
	}

	env.Pageservers = append(env.Pageservers, psConf)
	return nil
}

// provisionBranch lays out a fork's pair: allocate their ports, clone the
// pageserver's config from the ancestor with a Branch identity, and flush the
// ancestor up to the branch point. Mirrors Rust `provision_branch`.
//
// Like provisionBootstrap, nothing is started: the supervisor brings the pair up
// and creates the branch on the safekeeper once the pageserver reports Active.
// The ancestor flush belongs here because it is a request to a *different*
// endpoint's pageserver, which is already running.
func provisionBranch(ctx context.Context, env *config.LocalEnv, endpointID string, branchID, ancestorBranchID config.BranchID, startLSN config.Lsn, _ int) error {
	ancestorConf := env.PageserverConfForBranch(ancestorBranchID)
	if ancestorConf == nil {
		return fmt.Errorf("no pageserver serves ancestor branch %s", ancestorBranchID)
	}

	psConf, err := allocatePageserverConf(env, endpointID)
	if err != nil {
		return err
	}
	skConf, err := allocateSafekeeperConf(env, endpointID, branchID)
	if err != nil {
		return err
	}

	fmt.Printf("Provisioning safekeeper (pg port %d) for endpoint '%s'\n", skConf.PgPort, endpointID)
	if err := writeSafekeeperFiles(env, &skConf); err != nil {
		return err
	}
	env.Safekeepers = append(env.Safekeepers, skConf)

	fmt.Printf("Provisioning pageserver (pg %s) for endpoint '%s'\n", psConf.ListenPgAddr, endpointID)
	if err := os.MkdirAll(psConf.DataDir, 0o755); err != nil {
		return err
	}
	// Clone the ancestor's pageserver.toml, patch per-node bits.
	if err := clonePageserverFromTemplate(env, &psConf, ancestorConf, fmt.Sprintf("127.0.0.1:%d", skConf.PgPort)); err != nil {
		return err
	}
	branchIDCopy := branchID
	psConf.BranchID = &branchIDCopy
	if err := cfg.SetPageserverIdentity(filepath.Join(psConf.DataDir, "pageserver.toml"), branchID); err != nil {
		return err
	}

	// Best-effort ancestor flush up to the branch point (relies on the
	// branch-PS startup retry for correctness).
	ancestorPS := client.NewPageserver(ancestorConf.ListenHTTPAddr, httpClient())
	if err := ancestorPS.BranchFlush(ctx, ancestorBranchID, startLSN); err != nil {
		fmt.Printf("branch: best-effort ancestor flush failed (relying on startup retry): %v\n", err)
	}

	env.Pageservers = append(env.Pageservers, psConf)
	return nil
}

// writePageserverFiles writes a fresh pageserver.toml + metadata.json for a
// bootstrap pageserver. Mirrors Rust `pageserver_init`.
func writePageserverFiles(env *config.LocalEnv, psConf *config.PageServerConf, skConf *config.SafekeeperConf) error {
	initConf, err := pageserverInitConfFor(env, psConf)
	if err != nil {
		return err
	}
	skListen := "127.0.0.1"
	if skConf.ListenAddr != nil {
		skListen = *skConf.ListenAddr
	}
	params := cfg.PageserverParams{
		Conf:             initConf,
		PgDistribDir:     env.PgDistribDir,
		SafekeeperPgAddr: fmt.Sprintf("%s:%d", skListen, skConf.PgPort),
		ControlPlaneAPI:  env.ControlPlaneAPI,
	}
	if err := cfg.WritePageserverToml(filepath.Join(psConf.DataDir, "pageserver.toml"), params); err != nil {
		return err
	}
	return writePageserverMetadata(psConf)
}

// writePageserverMetadata writes metadata.json (the pageserver reads it at
// startup to self-register). Mirrors the NodeMetadata write in Rust
// `pageserver_init`.
func writePageserverMetadata(psConf *config.PageServerConf) error {
	httpPort, err := portOf(psConf.ListenHTTPAddr)
	if err != nil {
		return err
	}
	pgPort, err := portOf(psConf.ListenPgAddr)
	if err != nil {
		return err
	}
	meta := cfg.NodeMetadata{
		PostgresHost: "localhost",
		PostgresPort: pgPort,
		HTTPHost:     "localhost",
		HTTPPort:     httpPort,
	}
	if psConf.ListenHTTPSAddr != nil {
		hp, err := portOf(*psConf.ListenHTTPSAddr)
		if err != nil {
			return err
		}
		meta.HTTPSPort = &hp
	}
	if psConf.ListenGRPCAddr != nil {
		gp, err := portOf(*psConf.ListenGRPCAddr)
		if err != nil {
			return err
		}
		host := "localhost"
		meta.GrpcHost = &host
		meta.GrpcPort = &gp
	}
	return cfg.WriteMetadataJSON(filepath.Join(psConf.DataDir, "metadata.json"), meta)
}

// clonePageserverFromTemplate provisions a branch pageserver's datadir by
// cloning the ancestor's pageserver.toml and patching the per-node listen
// addresses + safekeeper address. The branch identity is baked afterwards by
// the caller. Mirrors Rust `PageServerNode::initialize_from_template`.
func clonePageserverFromTemplate(env *config.LocalEnv, psConf, ancestorConf *config.PageServerConf, safekeeperPgAddr string) error {
	templateTOML := filepath.Join(ancestorConf.DataDir, "pageserver.toml")
	data, err := os.ReadFile(templateTOML)
	if err != nil {
		return fmt.Errorf("read template %q: %w", templateTOML, err)
	}
	doc, err := unmarshalTOMLMap(data)
	if err != nil {
		return fmt.Errorf("parse template %q: %w", templateTOML, err)
	}
	doc["listen_pg_addr"] = psConf.ListenPgAddr
	doc["listen_http_addr"] = psConf.ListenHTTPAddr
	if psConf.ListenGRPCAddr != nil {
		doc["listen_grpc_addr"] = *psConf.ListenGRPCAddr
	} else {
		delete(doc, "listen_grpc_addr")
	}
	if psConf.ListenHTTPSAddr != nil {
		doc["listen_https_addr"] = *psConf.ListenHTTPSAddr
	} else {
		delete(doc, "listen_https_addr")
	}
	doc["safekeeper_pg_addr"] = safekeeperPgAddr
	// Strip any inherited identity: this node serves its own branch.
	delete(doc, "branch_id")
	delete(doc, "generation")

	out, err := marshalTOMLMap(doc)
	if err != nil {
		return err
	}
	tomlPath := filepath.Join(psConf.DataDir, "pageserver.toml")
	if err := os.WriteFile(tomlPath, out, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", tomlPath, err)
	}
	return nil
}
