package ui

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// CommandModel represents a Bubble Tea model for running external commands with a spinner
type CommandModel struct {
	spinner     spinner.Model
	output      []string
	done        bool
	err         error
	cmd         *exec.Cmd
	quitting    bool
	title       string
	description string
}

// CommandMsg types
type (
	commandOutputMsg string
	commandDoneMsg   struct {
		err error
	}
)

// NewCommandModel creates a new command model with a spinner
func NewCommandModel(cmd *exec.Cmd, title, description string) CommandModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ColorPrimary)

	return CommandModel{
		spinner:     s,
		output:      []string{},
		cmd:         cmd,
		title:       title,
		description: description,
	}
}

func (m CommandModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		runCommand(m.cmd),
	)
}

func runCommand(cmd *exec.Cmd) tea.Cmd {
	return func() tea.Msg {
		// Capture both stdout and stderr in a buffer
		var outBuf, errBuf strings.Builder
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf

		// Run the command and wait for it to complete
		err := cmd.Run()

		// Combine stdout and stderr
		output := outBuf.String()
		if errOutput := errBuf.String(); errOutput != "" {
			output += "\n" + errOutput
		}

		if err != nil {
			// Return both the error and the output
			return commandDoneMsg{err: fmt.Errorf("%w\n\nCommand output:\n%s", err, output)}
		}

		return commandDoneMsg{err: nil}
	}
}

func (m CommandModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.quitting = true
			return m, tea.Quit
		}

	case commandOutputMsg:
		m.output = append(m.output, string(msg))
		return m, nil

	case commandDoneMsg:
		m.done = true
		m.err = msg.err
		// Quit immediately when done
		return m, tea.Quit

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m CommandModel) View() string {
	if m.quitting || m.done {
		// Return empty when done so Bubble Tea clears completely
		return ""
	}

	var s strings.Builder

	// Show spinner and status while running
	s.WriteString(FormatTitle(m.title, ""))
	s.WriteString("\n\n")
	s.WriteString(m.spinner.View() + " " + m.description + "\n\n")

	return BoxStyle.Render(s.String())
}

// GetError returns the error from the command execution
func (m CommandModel) GetError() error {
	return m.err
}

// IsDone returns whether the command has completed
func (m CommandModel) IsDone() bool {
	return m.done
}
