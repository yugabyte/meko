// Package main runs LangfuseService.ProvisionForDatapack against one or
// more existing datapack ids that were created without a Langfuse
// project. The call is idempotent: if a mapping row already exists it
// is a no-op.
//
// Usage:
//
//	go run ./cmd/repair-langfuse-mapping <datapack_id> [<datapack_id> ...]
//
// Config comes from env vars (same names main.go reads):
//
//	SYSTEM_DB_HOST, SYSTEM_DB_PORT, SYSTEM_DB_NAME, SYSTEM_DB_USER_NAME,
//	SYSTEM_DB_PASSWORD, SYSTEM_DB_SSLMODE
//	LANGFUSE_HOST, LANGFUSE_ADMIN_API_KEY
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	_ "github.com/lib/pq"

	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: repair-langfuse-mapping <datapack_id> [<datapack_id> ...]")
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	sysDB, err := openDB(dsn(
		env("SYSTEM_DB_HOST", "127.0.0.1"),
		envInt("SYSTEM_DB_PORT", 5433),
		env("SYSTEM_DB_NAME", "meko_system"),
		env("SYSTEM_DB_USER_NAME", "yugabyte"),
		env("SYSTEM_DB_PASSWORD", ""),
		env("SYSTEM_DB_SSLMODE", "disable"),
	))
	if err != nil {
		slog.Error("database connection failed", "error", err)
		return 1
	}
	defer sysDB.Close()

	lf := service.NewLangfuseService(service.LangfuseConfig{
		SystemDB:    sysDB,
		DataDB:      nil,
		Host:        env("LANGFUSE_HOST", ""),
		AdminAPIKey: env("LANGFUSE_ADMIN_API_KEY", ""),
	})
	if !lf.Enabled() {
		slog.Error("langfuse service not enabled; set LANGFUSE_HOST and LANGFUSE_ADMIN_API_KEY")
		return 1
	}
	lfRepo := repository.NewLangfuseRepo(sysDB, nil, nil)
	accountRepo := repository.NewAccountRepo(sysDB, false)

	rc := 0
	for _, id := range os.Args[1:] {
		if err := repairOne(context.Background(), lf, lfRepo, accountRepo, id); err != nil {
			slog.Error("repair failed", "datapack_id", id, "error", err)
			rc = 1
			continue
		}
		slog.Info("repair ok", "datapack_id", id)
	}
	return rc
}

func repairOne(
	ctx context.Context,
	lf *service.LangfuseService,
	lfRepo *repository.LangfuseRepo,
	accountRepo *repository.AccountRepo,
	datapackID string,
) error {
	datapack, err := lfRepo.GetDatapackProvisioningInfo(datapackID)
	if err != nil {
		return fmt.Errorf("look up datapack: %w", err)
	}
	if datapack == nil {
		return fmt.Errorf("datapack not found")
	}
	acct, err := accountRepo.GetByID(ctx, datapack.OwnerUserID)
	if err != nil {
		return fmt.Errorf("look up owner: %w", err)
	}
	if acct == nil {
		return fmt.Errorf("owner %s not found", datapack.OwnerUserID)
	}
	slog.Info("preflight",
		"datapack_id", datapackID,
		"datapack_name", datapack.Name,
		"user_id", acct.ID,
		"email", acct.Email,
	)

	keys := lf.ProvisionForDatapack(acct, datapackID, datapack.Name)
	if keys == nil {
		return fmt.Errorf("ProvisionForDatapack returned nil (see prior log lines from the service)")
	}
	pkPrefix := keys.PublicKey
	if len(pkPrefix) > 8 {
		pkPrefix = pkPrefix[:8]
	}
	slog.Info("provisioned",
		"datapack_id", datapackID,
		"langfuse_public_key_prefix", pkPrefix,
	)
	return nil
}

func dsn(host string, port int, name, user, pw, sslmode string) string {
	return fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
		host, port, name, user, pw, sslmode,
	)
}

func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
