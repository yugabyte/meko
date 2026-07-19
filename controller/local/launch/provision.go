package launch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yugabyte/amp/controller/internal/registry"
	"github.com/yugabyte/amp/controller/local/cfg"
	"github.com/yugabyte/amp/controller/local/client"
	"github.com/yugabyte/amp/controller/local/config"
)

// lowestIDPageserverTemplate returns the min-id pageserver template.
func lowestIDPageserverTemplate(env *config.LocalEnv) (*config.YbAmpLocalInitPageserverConf, error) {
	if len(env.PageserverTemplates) == 0 {
		return nil, fmt.Errorf("no pageserver template configured")
	}
	best := &env.PageserverTemplates[0]
	for i := 1; i < len(env.PageserverTemplates); i++ {
		if env.PageserverTemplates[i].ID < best.ID {
			best = &env.PageserverTemplates[i]
		}
	}
	return best, nil
}

// lowestIDSafekeeperTemplate returns the min-id safekeeper template.
func lowestIDSafekeeperTemplate(env *config.LocalEnv) (*config.SafekeeperConf, error) {
	if len(env.SafekeeperTemplates) == 0 {
		return nil, fmt.Errorf("no safekeeper template configured")
	}
	best := &env.SafekeeperTemplates[0]
	for i := 1; i < len(env.SafekeeperTemplates); i++ {
		if env.SafekeeperTemplates[i].ID < best.ID {
			best = &env.SafekeeperTemplates[i]
		}
	}
	return best, nil
}

// allocatePageserverConf allocates the conf for a pageserver provisioned under
// endpointID: the next free id (the template's id when none are provisioned,
// so the first pageserver reuses it) with a per-id port delta. Mirrors Rust
// `allocate_pageserver_conf`.
func allocatePageserverConf(env *config.LocalEnv, endpointID string) (config.PageServerConf, error) {
	template, err := lowestIDPageserverTemplate(env)
	if err != nil {
		return config.PageServerConf{}, err
	}
	newID := template.ID
	for _, p := range env.Pageservers {
		if p.ID >= newID {
			newID = p.ID + 1
		}
	}
	if len(env.Pageservers) == 0 {
		newID = template.ID
	}
	delta := uint16((newID - template.ID) * 2)

	pgAddr, err := bumpAddrPort(template.ListenPgAddr, delta)
	if err != nil {
		return config.PageServerConf{}, err
	}
	httpAddr, err := bumpAddrPort(template.ListenHTTPAddr, delta)
	if err != nil {
		return config.PageServerConf{}, err
	}
	conf := config.PageServerConf{
		ID:             newID,
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
	template, err := lowestIDPageserverTemplate(env)
	if err != nil {
		return config.YbAmpLocalInitPageserverConf{}, err
	}
	conf := config.YbAmpLocalInitPageserverConf{
		ID:              allocated.ID,
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
	template, err := lowestIDSafekeeperTemplate(env)
	if err != nil {
		return config.SafekeeperConf{}, err
	}
	newID := template.ID
	for _, s := range env.Safekeepers {
		if s.ID >= newID {
			newID = s.ID + 1
		}
	}
	if len(env.Safekeepers) == 0 {
		newID = template.ID
	}
	delta := uint16((newID - template.ID) * 2)

	bid := branchID
	conf := config.SafekeeperConf{
		ID:              newID,
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

// allocateEndpointStorageConf allocates the endpoint_storage conf for
// endpointID: the lowest free port of the form template_base + 2k. Mirrors
// Rust `allocate_endpoint_storage_conf`.
func allocateEndpointStorageConf(env *config.LocalEnv, endpointID string) (config.EndpointStorageConf, error) {
	base, err := portOf(env.EndpointStorage.ListenAddr)
	if err != nil {
		return config.EndpointStorageConf{}, err
	}
	used := map[uint16]bool{}
	for _, e := range env.EndpointStorages {
		p, err := portOf(e.ListenAddr)
		if err != nil {
			return config.EndpointStorageConf{}, err
		}
		used[p] = true
	}
	port := base
	for used[port] {
		port += 2
	}
	addr, err := setAddrPort(env.EndpointStorage.ListenAddr, port)
	if err != nil {
		return config.EndpointStorageConf{}, err
	}
	return config.EndpointStorageConf{
		ListenAddr:        addr,
		RemoteStorageJSON: env.EndpointStorage.RemoteStorageJSON,
		EndpointID:        endpointID,
	}, nil
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

// createTimelineOnSafekeeper creates a timeline on the safekeeper co-located
// with the branch, at startLSN. Shared by bootstrap and branch provisioning.
func createTimelineOnSafekeeper(ctx context.Context, env *config.LocalEnv, conf *config.SafekeeperConf, branchID config.BranchID, pgVersion int, startLSN config.Lsn) error {
	listenAddr := "127.0.0.1"
	if conf.ListenAddr != nil {
		listenAddr = *conf.ListenAddr
	}
	c := client.NewSafekeeper(fmt.Sprintf("%s:%d", listenAddr, conf.HTTPPort), httpClient())
	return c.CreateTimeline(ctx, client.SafekeeperTimelineCreateRequest{
		BranchID: branchID,
		// The safekeeper's pg_version is a PgVersionId — the *full* PostgreSQL
		// version (major * 10000, e.g. 180000), not the major (18). Sending the
		// bare major makes the safekeeper panic (from_full_pg_version rejects it)
		// and drop the connection (EOF). Matches Rust PgVersionId::from(major).
		PgVersion: uint32(pgVersion) * 10000,
		StartLSN:  startLSN,
	})
}

// waitTimelineActive polls the pageserver until the branch's timeline is
// Active, returning its last_record_lsn. Mirrors the Active-wait loops in Rust
// `provision_bootstrap` / `provision_branch`.
func waitTimelineActive(ctx context.Context, conf *config.PageServerConf, branchID config.BranchID, timeout time.Duration) (config.Lsn, error) {
	c := client.NewPageserver(conf.ListenHTTPAddr, httpClient())
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("wait timeline active cancelled: %w", err)
		}
		info, err := c.TimelineInfo(ctx, branchID)
		if err == nil && info.State == "Active" {
			return info.LastRecordLSN, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", fmt.Errorf("branch did not become Active in time: %w", err)
			}
			return "", fmt.Errorf("branch did not become Active in time (state: %s)", info.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// provisionTimelineForEndpoint provisions the pageserver + safekeeper pair
// serving branchID, co-located under endpointID, consuming the pending init
// recorded at `branch create`. No-op when a pageserver already serves the
// branch. Mirrors Rust `provision_timeline_for_endpoint`.
func provisionTimelineForEndpoint(ctx context.Context, env *config.LocalEnv, endpointID string, branchID config.BranchID) error {
	if env.PageserverConfForBranch(branchID) != nil {
		return nil
	}
	base, err := controllerBase(env)
	if err != nil {
		return err
	}
	ctrl := client.NewController(base, httpClient())
	pending, err := ctrl.ListPendingInits(ctx)
	if err != nil {
		return err
	}
	var recipe *registry.PendingBranchInit
	for i := range pending {
		if pending[i].BranchID == branchID {
			recipe = &pending[i]
			break
		}
	}
	if recipe == nil {
		return fmt.Errorf(
			"branch %s has no provisioned pageserver and no pending init; run `branch create` first", branchID)
	}

	branchName := recipe.BranchName
	if recipe.AncestorBranchID == nil {
		if err := provisionBootstrap(ctx, env, endpointID, branchID, recipe.PgVersion, branchName); err != nil {
			return err
		}
	} else {
		if err := provisionBranch(ctx, env, endpointID, branchID, *recipe.AncestorBranchID, recipe.StartLSN, recipe.PgVersion, branchName); err != nil {
			return err
		}
	}

	// The pair is materialized; drop the pending recipe.
	return ctrl.DeletePendingInit(ctx, branchID)
}

// provisionBootstrap brings up the main branch's pair: start the safekeeper,
// start the pageserver (runs initdb + import), wait Active, create the
// timeline on the safekeeper at the bootstrap LSN, and register the branch.
// Mirrors Rust `provision_bootstrap`.
func provisionBootstrap(ctx context.Context, env *config.LocalEnv, endpointID string, branchID config.BranchID, pgVersion int, branchName string) error {
	psConf, err := allocatePageserverConf(env, endpointID)
	if err != nil {
		return err
	}
	skConf, err := allocateSafekeeperConf(env, endpointID, branchID)
	if err != nil {
		return err
	}
	env.Safekeepers = append(env.Safekeepers, skConf)

	fmt.Printf("Provisioning safekeeper %d (pg port %d) for endpoint '%s'\n", skConf.ID, skConf.PgPort, endpointID)
	if err := startSafekeeper(env, &skConf, 30*time.Second); err != nil {
		return fmt.Errorf("start main safekeeper: %w", err)
	}

	// On any failure past this point the config mutation is discarded, so a
	// retry reuses these ports — stop the daemons we started so it doesn't
	// collide with orphans. stop* on a not-yet-started daemon is a no-op.
	success := false
	defer func() {
		if !success {
			_ = stopSafekeeper(env, &skConf, true)
			_ = stopPageserver(&psConf, true)
		}
	}()

	fmt.Printf("Provisioning pageserver %d (pg %s) for endpoint '%s'\n", psConf.ID, psConf.ListenPgAddr, endpointID)
	if err := os.MkdirAll(psConf.DataDir, 0o755); err != nil {
		return err
	}
	if err := writePageserverFiles(env, &psConf, &skConf); err != nil {
		return err
	}
	// Bake the Bootstrap identity, then start (register + spawn).
	init := cfg.BootstrapInit(pgVersion)
	branchIDCopy := branchID
	psConf.BranchID = &branchIDCopy
	if err := cfg.SetPageserverIdentity(filepath.Join(psConf.DataDir, "pageserver.toml"), branchID, 0, &init); err != nil {
		return err
	}
	if err := startPageserver(ctx, env, &psConf, 60*time.Second); err != nil {
		return fmt.Errorf("start main pageserver: %w", err)
	}

	startLSN, err := waitTimelineActive(ctx, &psConf, branchID, 120*time.Second)
	if err != nil {
		return err
	}

	if err := createTimelineOnSafekeeper(ctx, env, &skConf, branchID, pgVersion, startLSN); err != nil {
		return fmt.Errorf("create bootstrap timeline on safekeeper: %w", err)
	}

	base, err := controllerBase(env)
	if err != nil {
		return err
	}
	bn := branchName
	if err := client.NewController(base, httpClient()).BranchRegister(ctx, branchID, registry.BranchRegisterRequest{
		StartLSN:   startLSN,
		BranchName: &bn,
	}); err != nil {
		return fmt.Errorf("register bootstrap branch with controller: %w", err)
	}

	env.Pageservers = append(env.Pageservers, psConf)
	success = true
	return nil
}

// provisionBranch brings up a fork's pair: create the safekeeper at the branch
// LSN, start the pageserver cloned from the ancestor with a Branch identity,
// wait Active, and register the branch. Mirrors Rust `provision_branch`.
func provisionBranch(ctx context.Context, env *config.LocalEnv, endpointID string, branchID, ancestorBranchID config.BranchID, startLSN config.Lsn, pgVersion int, branchName string) error {
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
	env.Safekeepers = append(env.Safekeepers, skConf)

	fmt.Printf("Provisioning safekeeper %d (pg port %d) for endpoint '%s'\n", skConf.ID, skConf.PgPort, endpointID)
	if err := startSafekeeper(env, &skConf, 30*time.Second); err != nil {
		return fmt.Errorf("start branch safekeeper: %w", err)
	}

	// On any failure past this point the config mutation is discarded, so a
	// retry reuses these ports — stop the daemons we started. stop* on a
	// not-yet-started daemon is a no-op.
	success := false
	defer func() {
		if !success {
			_ = stopSafekeeper(env, &skConf, true)
			_ = stopPageserver(&psConf, true)
		}
	}()

	if err := createTimelineOnSafekeeper(ctx, env, &skConf, branchID, pgVersion, startLSN); err != nil {
		return fmt.Errorf("create branch timeline on safekeeper: %w", err)
	}

	fmt.Printf("Provisioning pageserver %d (pg %s) for endpoint '%s'\n", psConf.ID, psConf.ListenPgAddr, endpointID)
	if err := os.MkdirAll(psConf.DataDir, 0o755); err != nil {
		return err
	}
	// Clone the ancestor's pageserver.toml, patch per-node bits.
	if err := clonePageserverFromTemplate(env, &psConf, ancestorConf, fmt.Sprintf("127.0.0.1:%d", skConf.PgPort)); err != nil {
		return err
	}
	init := cfg.BranchInit(ancestorBranchID, startLSN)
	branchIDCopy := branchID
	psConf.BranchID = &branchIDCopy
	if err := cfg.SetPageserverIdentity(filepath.Join(psConf.DataDir, "pageserver.toml"), branchID, 0, &init); err != nil {
		return err
	}

	// Best-effort ancestor flush up to the branch point (relies on the
	// branch-PS startup retry for correctness).
	ancestorPS := client.NewPageserver(ancestorConf.ListenHTTPAddr, httpClient())
	if err := ancestorPS.BranchFlush(ctx, ancestorBranchID, startLSN); err != nil {
		fmt.Printf("branch: best-effort ancestor flush failed (relying on startup retry): %v\n", err)
	}

	if err := startPageserver(ctx, env, &psConf, 60*time.Second); err != nil {
		return fmt.Errorf("start branch pageserver: %w", err)
	}
	if _, err := waitTimelineActive(ctx, &psConf, branchID, 120*time.Second); err != nil {
		return err
	}

	base, err := controllerBase(env)
	if err != nil {
		return err
	}
	bn := branchName
	if err := client.NewController(base, httpClient()).BranchRegister(ctx, branchID, registry.BranchRegisterRequest{
		StartLSN:   startLSN,
		BranchName: &bn,
	}); err != nil {
		return fmt.Errorf("register branch with controller: %w", err)
	}

	env.Pageservers = append(env.Pageservers, psConf)
	success = true
	return nil
}

// writePageserverFiles writes a fresh pageserver.toml + identity.toml +
// metadata.json for a bootstrap pageserver. Mirrors Rust `pageserver_init`.
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
	if err := cfg.WriteIdentityToml(filepath.Join(psConf.DataDir, "identity.toml"), psConf.ID); err != nil {
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
// addresses + safekeeper address, then writes identity.toml. The branch
// identity is baked afterwards by the caller. Mirrors Rust
// `PageServerNode::initialize_from_template`.
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
	delete(doc, "timeline_init")

	out, err := marshalTOMLMap(doc)
	if err != nil {
		return err
	}
	tomlPath := filepath.Join(psConf.DataDir, "pageserver.toml")
	if err := os.WriteFile(tomlPath, out, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", tomlPath, err)
	}
	return cfg.WriteIdentityToml(filepath.Join(psConf.DataDir, "identity.toml"), psConf.ID)
}
