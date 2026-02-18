package ui

import (
	"fmt"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// RunCommandWithUI runs a command with a Bubble Tea spinner UI
// Returns error if the command fails
func RunCommandWithUI(cmd *exec.Cmd, title, description string) error {
	// Create and run the Bubble Tea program
	model := NewCommandModel(cmd, title, description)
	p := tea.NewProgram(
		model,
		tea.WithInput(nil), // Don't require input
	)

	finalModel, err := p.Run()

	if err != nil {
		// If Bubble Tea fails (no TTY), fallback to simple mode
		return runCommandSimple(cmd, title)
	}

	// Check if the command was successful
	if m, ok := finalModel.(CommandModel); ok {
		if m.err != nil {
			// Print error in a box after Bubble Tea exits
			fmt.Println()
			errorBox := BoxStyle.Render(
				ErrorStyle.Render(EmojiCross + " Error: " + m.err.Error()),
			)
			fmt.Println(errorBox)
			fmt.Println()
			return fmt.Errorf("command failed: %w", m.err)
		}
		// Print success message in a box after Bubble Tea exits
		fmt.Println()
		successBox := BoxStyle.Render(
			SuccessStyle.Render(EmojiCheckmark+" "+m.description+" completed successfully! "+EmojiParty) +
				"\n\n" +
				DividerLine(80),
		)
		fmt.Println(successBox)
		fmt.Println()
	}

	return nil
}

// runCommandSimple runs the command without Bubble Tea UI (fallback mode)
func runCommandSimple(cmd *exec.Cmd, title string) error {
	fmt.Printf("\n%s\n\n", FormatTitle(EmojiRocket, title))

	if cmd.Stdout != nil {
		// Command already has output configured, just run it
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("command failed: %w", err)
		}
	} else {
		// Capture output
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Println(FormatError("Command failed"))
			if len(out) > 0 {
				fmt.Println(string(out))
			}
			return fmt.Errorf("command failed: %w", err)
		}
		if len(out) > 0 {
			fmt.Println(string(out))
		}
	}

	return nil
}

// PrintSuccess prints a success message
func PrintSuccess(message string) {
	fmt.Printf("\n%s\n", FormatSuccess(message))
	fmt.Printf("\n%s\n\n", DividerLine(80))
}

// PrintError prints an error message
func PrintError(message string) {
	fmt.Printf("\n%s\n\n", FormatError(message))
}

// PrintWarning prints a warning message
func PrintWarning(message string) {
	fmt.Printf("\n%s\n\n", FormatWarning(message))
}

// PrintInfo prints an info message
func PrintInfo(message string) {
	fmt.Printf("\n%s\n\n", FormatInfo(message))
}
