package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	_ "github.com/lib/pq" // registers the postgres driver for database/sql
	"github.com/spf13/cobra"

	"yuga-cli/internal/ui"
)

var inferenceNode bool
var hostAddress string

// PlatformContext holds shared context for all startup functions
type PlatformContext struct {
	platformDir string
	logDir      string
	pid         int
}

func getPlatformDir() (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}
	execDir := filepath.Dir(execPath)
	return filepath.Dir(execDir), nil
}

func startYugabyteDBNodeSilent(ctx *PlatformContext) ([]string, error) {
	platformDir, err := getPlatformDir()
	if err != nil {
		return nil, err
	}
	ctx.platformDir = platformDir

	// Find the yugabyte-* directory
	pattern := filepath.Join(platformDir, "yugabyte-*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to search for yugabyte directory: %w", err)
	}

	// Filter to only directories
	var yugabyteDirs []string
	for _, match := range matches {
		if info, err := os.Stat(match); err == nil && info.IsDir() {
			yugabyteDirs = append(yugabyteDirs, match)
		}
	}

	if len(yugabyteDirs) == 0 {
		return nil, fmt.Errorf("no yugabyte-* directory found in %s", platformDir)
	}

	// Use the first yugabyte directory found
	yugabyteDir := yugabyteDirs[0]
	yugabytedBin := filepath.Join(yugabyteDir, "bin", "yugabyted")

	// Check if yugabyted binary exists
	if _, err := os.Stat(yugabytedBin); os.IsNotExist(err) {
		return nil, fmt.Errorf("yugabyted binary not found at %s", yugabytedBin)
	}

	// Build the command arguments
	args := []string{
		"start",
		"--advertise_address", hostAddress,
		"--master_flags=master_auto_run_initdb=true,time_source=\"\"",
		"--tserver_flags=time_source=\"\"," +
			"enable_dist_rag_service=true," +
			"dist_rag_conf_csv={" +
			"AWS_S3_BUCKET_NAME=" + os.Getenv("AWS_S3_BUCKET_NAME") + "," +
			"AWS_REGION=" + os.Getenv("AWS_REGION") + "," +
			"AWS_ACCESS_KEY_ID=" + os.Getenv("AWS_ACCESS_KEY_ID") + "," +
			"AWS_SECRET_ACCESS_KEY=" + os.Getenv("AWS_SECRET_ACCESS_KEY") + "," +
			"OPENAI_API_KEY=" + os.Getenv("OPENAI_API_KEY") + "}",
	}

	// Create the command
	cmd := exec.Command(yugabytedBin, args...)

	// Run the command
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	details := []string{
		fmt.Sprintf("Node address: %s:5433", hostAddress),
		"YSQL API enabled",
	}

	return details, nil
}

func configureDatabaseForPlatformSilent(_ *PlatformContext) ([]string, error) {
	// Build connection string using the host address
	connString := fmt.Sprintf("postgresql://%s:5433/yugabyte?user=yugabyte&password=&sslmode=disable", hostAddress)

	// Wait for database to be ready (retry logic)
	maxRetries := 3
	retryDelay := 2 * time.Second

	var db *sql.DB
	var err error

	// Retry connecting to the database
	for attempt := 1; attempt <= maxRetries; attempt++ {
		db, err = sql.Open("postgres", connString)
		if err != nil {
			if attempt < maxRetries {
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf("failed to open database connection after %d attempts: %w", maxRetries, err)
		}

		// Test the connection with context timeout
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = db.PingContext(ctx2)
		cancel()

		if err != nil {
			db.Close()
			if attempt < maxRetries {
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf("failed to connect to YugabyteDB after %d attempts: %w", maxRetries, err)
		}

		// Successfully connected
		break
	}

	defer db.Close()

	var details []string

	// Check if pgvector extension already exists
	var exists bool
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'vector')").Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("error checking for pgvector extension: %w", err)
	}

	if exists {
		details = append(details, "Extensions already configured")
	} else {
		// Create pgvector and pg_dist_rag extensions
		_, err = db.Exec("CREATE EXTENSION vector")
		if err != nil {
			return nil, fmt.Errorf("error creating pgvector extension: %w", err)
		}

		_, err = db.Exec("CREATE EXTENSION pg_dist_rag")
		if err != nil {
			return nil, fmt.Errorf("error creating pg_dist_rag extension: %w", err)
		}

		_, err = db.Exec("CREATE EXTENSION age")
		if err != nil {
			return nil, fmt.Errorf("error creating Apache AGE extension: %w", err)
		}

		details = append(details, "Created pgvector extension")
		details = append(details, "Created pg_dist_rag extension")
		details = append(details, "Created Apache AGE extension")
	}

	// Create agents table if it doesn't exist
	createTableSQL := `
		CREATE TABLE IF NOT EXISTS agents (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			agent_name VARCHAR(255),
			agent_metadata JSONB
		);
	`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		return nil, fmt.Errorf("error creating agents table: %w", err)
	}

	details = append(details, "Configured agents table")

	return details, nil
}

func setupSystemAgentSilent(ctx *PlatformContext) ([]string, error) {
	// Path to system agent directory
	systemAgentDir := filepath.Join(ctx.platformDir, "system_agent")
	streamlitAppPath := filepath.Join(systemAgentDir, "ui", "app.py")

	// Check if Streamlit app exists
	if _, err := os.Stat(streamlitAppPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("streamlit app not found at %s", streamlitAppPath)
	}

	// Create log directory
	ctx.logDir = filepath.Join(ctx.platformDir, "logs")
	if err := os.MkdirAll(ctx.logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	var details []string

	// Setup Python virtual environment
	venvDir := filepath.Join(systemAgentDir, "venv")
	pythonBin := filepath.Join(venvDir, "bin", "python3")
	pipBin := filepath.Join(venvDir, "bin", "pip")
	streamlitBin := filepath.Join(venvDir, "bin", "streamlit")

	// Check if venv exists, if not create it
	if _, err := os.Stat(venvDir); os.IsNotExist(err) {
		// Create virtual environment
		cmd := exec.Command("python3", "-m", "venv", venvDir)
		cmd.Dir = systemAgentDir
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("failed to create virtual environment: %w", err)
		}
		details = append(details, "Created Python virtual environment")

		// Upgrade pip
		cmd = exec.Command(pythonBin, "-m", "pip", "install", "--upgrade", "pip")
		cmd.Dir = systemAgentDir
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("failed to upgrade pip: %w", err)
		}

		// Install dependencies from requirements.txt
		requirementsPath := filepath.Join(systemAgentDir, "requirements.txt")
		if _, err := os.Stat(requirementsPath); err == nil {
			cmd = exec.Command(pipBin, "install", "-r", requirementsPath)
			cmd.Dir = systemAgentDir
			if err := cmd.Run(); err != nil {
				return nil, fmt.Errorf("failed to install dependencies: %w", err)
			}
			details = append(details, "Installed Python dependencies")
		}
	} else {
		// Venv exists, check if we need to update dependencies
		// For now, we'll skip updating to make startup faster
		details = append(details, "Using existing Python environment")
	}

	// Verify streamlit binary exists
	if _, err := os.Stat(streamlitBin); os.IsNotExist(err) {
		return nil, fmt.Errorf("streamlit binary not found at %s (venv may be corrupted)", streamlitBin)
	}

	// Create log file paths
	streamlitStdout := filepath.Join(ctx.logDir, "streamlit_stdout.log")
	streamlitStderr := filepath.Join(ctx.logDir, "streamlit_stderr.log")

	// Open log files (DO NOT defer close - let them stay open for the background process)
	stdoutFile, err := os.OpenFile(streamlitStdout, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open stdout log file: %w", err)
	}

	stderrFile, err := os.OpenFile(streamlitStderr, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		stdoutFile.Close()
		return nil, fmt.Errorf("failed to open stderr log file: %w", err)
	}

	// Prepare Streamlit command using venv's streamlit
	cmd := exec.Command(streamlitBin, "run", streamlitAppPath, "--server.headless", "true")
	cmd.Dir = ctx.platformDir
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile

	// Set up environment variables to use the venv
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("VIRTUAL_ENV=%s", venvDir),
		fmt.Sprintf("PATH=%s:%s", filepath.Join(venvDir, "bin"), os.Getenv("PATH")),
	)

	// Start the process in the background
	if err := cmd.Start(); err != nil {
		stdoutFile.Close()
		stderrFile.Close()
		return nil, fmt.Errorf("failed to start streamlit: %w", err)
	}

	// Get the PID
	ctx.pid = cmd.Process.Pid

	// Save PID to file
	pidFile := filepath.Join(ctx.logDir, "streamlit.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", ctx.pid)), 0600); err != nil {
		return nil, fmt.Errorf("failed to write PID file: %w", err)
	}

	// Give it a moment to start
	time.Sleep(2 * time.Second)

	// Check if process is still running by checking /proc/<pid>
	procPath := fmt.Sprintf("/proc/%d", ctx.pid)
	if _, err := os.Stat(procPath); os.IsNotExist(err) {
		stdoutFile.Close()
		stderrFile.Close()
		return nil, fmt.Errorf("streamlit process exited immediately (PID %d). Check logs at %s and %s",
			ctx.pid, streamlitStdout, streamlitStderr)
	}

	// Note: We intentionally DO NOT close stdoutFile and stderrFile here
	// They need to remain open for the background Streamlit process to write to them

	details = append(details, fmt.Sprintf("PID: %d", ctx.pid))
	details = append(details, fmt.Sprintf("UI: http://%s:8501", hostAddress))

	return details, nil
}

func startYugaAIPlatform() error {
	ctx := &PlatformContext{}

	// Create and run the orchestrator with Bubble Tea
	model := ui.NewPlatformStartupModel()
	p := tea.NewProgram(model, tea.WithInput(nil))

	// Run startup steps in a goroutine and send messages to the model
	go func() {
		// Step 1: Start YugabyteDB
		p.Send(ui.StepStartMsg{Step: ui.StepYugabyteDB})
		details, err := startYugabyteDBNodeSilent(ctx)
		p.Send(ui.StepCompleteMsg{
			Step:    ui.StepYugabyteDB,
			Details: details,
			Err:     err,
		})
		if err != nil {
			return
		}

		// Step 2: Configure Database
		p.Send(ui.StepStartMsg{Step: ui.StepDatabaseConfig})
		details, err = configureDatabaseForPlatformSilent(ctx)
		p.Send(ui.StepCompleteMsg{
			Step:    ui.StepDatabaseConfig,
			Details: details,
			Err:     err,
		})
		if err != nil {
			return
		}

		// Step 3: Setup System Agent
		p.Send(ui.StepStartMsg{Step: ui.StepSystemAgent})
		details, err = setupSystemAgentSilent(ctx)
		p.Send(ui.StepCompleteMsg{
			Step:    ui.StepSystemAgent,
			Details: details,
			Err:     err,
		})
		if err != nil {
			return
		}

		// All done - send platform info
		p.Send(ui.PlatformCompleteMsg{
			Info: ui.PlatformInfo{
				PID:         ctx.pid,
				HostAddress: hostAddress,
			},
		})
	}()

	// Run the Bubble Tea program
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("UI error: %w", err)
	}

	// Check if there was an error during startup
	if m, ok := finalModel.(ui.PlatformStartupModel); ok {
		if m.GetError() != nil {
			fmt.Println()
			errorBox := ui.BoxStyle.Render(
				ui.ErrorStyle.Render(ui.EmojiCross + " Error: " + m.GetError().Error()),
			)
			fmt.Println(errorBox)
			fmt.Println()
			return m.GetError()
		}

		// Print final success status
		platformInfo := m.GetPlatformInfo()
		printFinalStatus(platformInfo)
	}

	return nil
}

func printFinalStatus(info ui.PlatformInfo) {
	statusContent := lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.NewStyle().Bold(true).Foreground(ui.ColorPrimary).Render("Meko Intelligence Platform"),
		"",
		lipgloss.JoinHorizontal(lipgloss.Left,
			lipgloss.NewStyle().Width(20).Foreground(ui.ColorInfo).Render("Status"),
			": ",
			lipgloss.NewStyle().Foreground(ui.ColorSuccess).Render("Running"),
		),
		lipgloss.JoinHorizontal(lipgloss.Left,
			lipgloss.NewStyle().Width(20).Foreground(ui.ColorInfo).Render("PID"),
			": ",
			fmt.Sprintf("%d", info.PID),
		),
		lipgloss.JoinHorizontal(lipgloss.Left,
			lipgloss.NewStyle().Width(20).Foreground(ui.ColorInfo).Render("Application"),
			": ",
			"System Agent UI",
		),
		lipgloss.JoinHorizontal(lipgloss.Left,
			lipgloss.NewStyle().Width(20).Foreground(ui.ColorInfo).Render("UI URL"),
			": ",
			lipgloss.NewStyle().Foreground(ui.ColorPrimary).Underline(true).
				Render(fmt.Sprintf("http://%s:8501", info.HostAddress)),
		),
		"",
		ui.DividerLine(80),
		"",
		lipgloss.NewStyle().Foreground(ui.ColorSuccess).Bold(true).Render(ui.EmojiRocket+" meko started successfully!"),
	)

	statusBox := ui.BoxStyle.Render(statusContent)
	fmt.Println()
	fmt.Println(statusBox)
	fmt.Println()
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Yuga AI Platform",
	Long:  `Start the Yuga AI Platform`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if inferenceNode {
			ui.PrintInfo("Starting the Yuga AI Platform with inference stack enabled")
			// TODO: Implement inference stack startup
		}
		return startYugaAIPlatform()
	},
}

func init() {
	rootCmd.AddCommand(startCmd)
	startCmd.Flags().BoolVar(&inferenceNode, "inference_node", false, "Start the inference stack")
	startCmd.Flags().StringVar(&hostAddress, "host", "127.0.0.1",
		"Host address (IP or DNS) for all external service endpoints and connections")
}
