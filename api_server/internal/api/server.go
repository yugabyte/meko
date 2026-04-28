package api

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ServerConfig holds the configuration for the API server.
type ServerConfig struct {
	DB                   *sql.DB
	CognitoRegion        string
	CognitoPoolID        string
	CognitoClientID      string
	CognitoClientSecret  string
	CognitoDomain        string
	LangfuseHost         string
	LangfuseAdminAPIKey  string
	// Optional Langfuse Postgres pool. Nil when unconfigured; dependent
	// endpoints return 503.
	LangfuseDB           *sql.DB
	OpenAIAPIKey         string
	OpenAILLMModel       string
	OpenAIEmbeddingModel string
	Port                 int
	CRDNamespace         string
	S3AccessKey         string
	S3AuthMode          string
	S3Bucket            string
	S3Endpoint          string
	S3Region            string
	S3SecretKey         string
}

// Run starts the Echo server and blocks until a shutdown signal is received.
func Run(cfg ServerConfig) error {
	e, err := NewRouter(cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize router: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		addr := fmt.Sprintf(":%d", cfg.Port)
		slog.Info("api server starting", "port", cfg.Port)
		if err := e.Start(addr); err != nil {
			errCh <- fmt.Errorf("server error: %w", err)
		}
		close(errCh)
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	slog.Info("shutting down api server")
	if err := e.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown error: %w", err)
	}

	slog.Info("api server stopped")
	return nil
}
