package cmd

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"yuga-cli/internal/tui"
)

var (
	// Flags
	verbose bool
	cfgFile string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "yuga-cli",
	Short: "A powerful TUI-based CLI tool for Yuga AI Platform",
	Long: `Yuga CLI is a command-line interface tool that provides an interactive
Terminal User Interface (TUI) for managing and interacting with the Yuga AI Platform.

Built with Cobra and Bubble Tea, it offers a modern, responsive CLI experience
similar to Sourcegraph's AMP CLI.`,
	Version: "0.1.0",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Launch interactive TUI when no subcommand is provided
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
	cobra.OnInitialize(initConfig)

	// Persistent flags (available to all subcommands)
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.yuga-cli.yaml)")
}

func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag
		fmt.Printf("Using config file: %s\n", cfgFile)
	}
}

// exitError prints the error and exits with code 1
func exitError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
