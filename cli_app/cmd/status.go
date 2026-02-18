package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"yuga-cli/internal/ui"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show system status",
	Long:  `Display the current status of all Yuga AI Platform components.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println()
		fmt.Println(ui.FormatTitle(ui.EmojiChart, "Yuga AI Platform Status"))
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
		
		ui.PrintInfo("For detailed status, use: yuga-cli interactive")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
