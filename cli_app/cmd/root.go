package cmd

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"cli_app/internal/config"
	"cli_app/internal/logger"
	"cli_app/internal/tui"
)

var (
	verbose   bool
	appConfig *config.Config
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "meko",
	Short: "A powerful TUI-based CLI tool for meko AI Platform",
	Long: `Meko is a command-line interface tool that provides an interactive
Terminal User Interface (TUI) for managing and interacting with the meko AI Platform.

Built with Cobra and Bubble Tea, it offers a modern, responsive CLI experience
similar to Sourcegraph's AMP CLI.`,
	Version: "0.1.0",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		appConfig = cfg
		return logger.Init(cfg.LogDir, verbose)
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		logger.Close()
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		p := tea.NewProgram(
			tui.NewModel(),
			tea.WithAltScreen(),
			tea.WithMouseCellMotion(),
		)

		if _, err := p.Run(); err != nil {
			return fmt.Errorf("failed to run TUI: %w", err)
		}

		return nil
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")
}
