package cmd

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"cli_app/internal/tui"
)

var interactiveCmd = &cobra.Command{
	Use:   "interactive",
	Short: "Launch interactive TUI mode",
	Long: `Launch the interactive Terminal User Interface (TUI) for Meko Platform.

This provides a live, interactive experience with menu navigation, real-time updates,
and a beautiful interface powered by Bubble Tea.`,
	Aliases: []string{"i", "tui", "ui"},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create and run the TUI
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

func init() {
	// todo: add back in for meko lite
	// rootCmd.AddCommand(interactiveCmd)
}
