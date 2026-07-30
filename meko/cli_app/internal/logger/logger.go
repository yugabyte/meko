package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

var (
	defaultLogger *slog.Logger
	logFile       *os.File
)

func DefaultLogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, "var", "meko", "logs")
}

// Init sets up the global logger. It creates logDir if needed, opens a
// timestamped log file, and configures slog as the default logger.
// When verbose is true, log output is also tee'd to stderr.
func Init(logDir string, verbose bool) error {
	if logDir == "" {
		logDir = DefaultLogDir()
	}

	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("create log directory %s: %w", logDir, err)
	}

	filename := fmt.Sprintf("meko_%s.log", time.Now().Format("20060102"))
	path := filepath.Join(logDir, filename)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", path, err)
	}
	logFile = f

	var w io.Writer = f
	if verbose {
		w = io.MultiWriter(f, os.Stderr)
	}

	defaultLogger = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(defaultLogger)

	slog.Info("logger initialized", "log_dir", logDir, "file", path)
	return nil
}

// LogDir returns the directory of the currently active log file,
// or the default if the logger hasn't been initialized yet.
func LogDir() string {
	if logFile != nil {
		return filepath.Dir(logFile.Name())
	}
	return DefaultLogDir()
}

func Close() {
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}
