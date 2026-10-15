// Command datapack-metrics-consumer reads the durable event log, meko_system.events, and applies each event's
// effects. See README.md.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq" // registers the "postgres" driver

	"meko/event-consumers/datapack-metrics-consumer/internal/config"
	"meko/event-consumers/datapack-metrics-consumer/internal/consumer"
	"meko/event-consumers/datapack-metrics-consumer/internal/handlers"
	"meko/event-consumers/datapack-metrics-consumer/internal/health"
	"meko/event-consumers/datapack-metrics-consumer/internal/store"
)

func main() {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToLower(os.Getenv("LOG_LEVEL")))); err != nil {
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		slog.Error("bad configuration", "error", err)
		os.Exit(1)
	}
	db, err := sql.Open("postgres", cfg.DSN)
	if err != nil {
		slog.Error("cannot open the database", "error", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()
	// One connection, used by one goroutine; a transaction holds it, so nothing else may use db meanwhile.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)

	checkTimeZone(db)
	reg := consumer.NewRegistry()
	if err := handlers.Register(reg, cfg.Schema); err != nil {
		slog.Error("cannot register the handlers", "error", err)
		os.Exit(1)
	}
	c := consumer.New(cfg, db, reg, uuid.NewString())
	shutdown, err := health.Serve(cfg.HTTPPort, c.Heartbeat())
	if err != nil {
		slog.Error("cannot start the health server", "error", err)
		os.Exit(1)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		c.Stop()
	}()

	slog.Info("events consumer starting", "id", c.ID(), "group", cfg.Consumer, "shards", len(cfg.Shards),
		"batch", cfg.BatchSize, "safety_lag", cfg.SafetyLag, "handlers", c.Handlers())
	c.Run(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

// checkTimeZone warns when the database's time zone is not UTC. The handler counts a search toward the UTC
// date of its occurred_at, while the inline code and the readers of datapack_daily_activity use CURRENT_DATE
// in the session time zone, so searches near midnight would land on different days in the two paths.
func checkTimeZone(db *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tz, err := store.ServerTimeZone(ctx, db)
	switch {
	case err != nil:
		slog.Warn("could not read the database time zone", "error", err)
	case strings.EqualFold(tz, "UTC"), strings.EqualFold(tz, "Etc/UTC"):
		slog.Info("database time zone", "timezone", tz)
	default:
		slog.Warn("the database time zone is not UTC: daily counts from this consumer use UTC dates, "+
			"while the inline path and the dashboard readers use CURRENT_DATE in this zone", "timezone", tz)
	}
}
