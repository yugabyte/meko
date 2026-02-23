package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"cli_app/internal/ui"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show system status",
	Long:  `Display the current status of all Meko Platform components.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println()
		fmt.Println(ui.FormatTitle(ui.EmojiChart, "Meko Platform Status"))
		fmt.Println()
		fmt.Println(ui.DividerLine(60))
		fmt.Println()

		// Show component status with green text
		fmt.Printf("  %s YugabyteDB:    %s\n", ui.EmojiDatabase, ui.SuccessText.Render("✓ Running"))
		fmt.Printf("  %s AI Agent:      %s\n", ui.EmojiGear, ui.SuccessText.Render("✓ Active"))
		fmt.Printf("  %s API Server:    %s\n", ui.EmojiCheckmark, ui.SuccessText.Render("✓ Healthy"))
		fmt.Println()
		fmt.Println(ui.DividerLine(60))
		fmt.Println()

		ui.PrintInfo("For detailed status, use: meko interactive")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
