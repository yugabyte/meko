package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PlatformStartupStep represents a single startup step
type PlatformStartupStep int

const (
	StepYugabyteDB PlatformStartupStep = iota
	StepDatabaseConfig
	StepSystemAgent
	StepComplete
)

// StepInfo holds information about a startup step
type StepInfo struct {
	Name        string
	Description string
	Status      string // "pending", "running", "success", "error"
	Error       error
	Details     []string
}

// PlatformStartupModel represents the Bubble Tea model for platform startup
type PlatformStartupModel struct {
	err          error
	steps        []StepInfo
	platformInfo PlatformInfo
	spinner      spinner.Model
	currentStep  PlatformStartupStep
	done         bool
	quitting     bool
}

// PlatformInfo contains information to display at the end
type PlatformInfo struct {
	HostAddress string
	PID         int
}

// Message types for platform startup
type (
	StepStartMsg struct {
		Step PlatformStartupStep
	}
	StepCompleteMsg struct {
		Err     error
		Details []string
		Step    PlatformStartupStep
	}
	PlatformCompleteMsg struct {
		Info PlatformInfo
	}
)

// NewPlatformStartupModel creates a new platform startup model
func NewPlatformStartupModel() PlatformStartupModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ColorPrimary)

	steps := []StepInfo{
		{
			Name:        "YugabyteDB",
			Description: "Starting YugabyteDB node",
			Status:      "pending",
		},
		{
			Name:        "Database Configuration",
			Description: "Setting up database extensions and tables",
			Status:      "pending",
		},
		{
			Name:        "System Agent",
			Description: "Starting system agent UI",
			Status:      "pending",
		},
	}

	return PlatformStartupModel{
		spinner:     s,
		currentStep: StepYugabyteDB,
		steps:       steps,
		done:        false,
	}
}

func (m PlatformStartupModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			return StepStartMsg{Step: StepYugabyteDB}
		},
	)
}

func (m PlatformStartupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.quitting = true
			return m, tea.Quit
		}

	case StepStartMsg:
		// Mark current step as running
		if int(msg.Step) < len(m.steps) {
			m.steps[msg.Step].Status = "running"
			m.currentStep = msg.Step
		}
		return m, nil

	case StepCompleteMsg:
		// Mark step as complete or error
		if int(msg.Step) < len(m.steps) {
			if msg.Err != nil {
				m.steps[msg.Step].Status = "error"
				m.steps[msg.Step].Error = msg.Err
				m.err = msg.Err
				m.done = true
				return m, tea.Quit
			}
			m.steps[msg.Step].Status = "success"
			m.steps[msg.Step].Details = msg.Details
		}

		// Move to next step or complete
		nextStep := msg.Step + 1
		if nextStep < StepComplete {
			return m, func() tea.Msg {
				return StepStartMsg{Step: nextStep}
			}
		}
		return m, nil

	case PlatformCompleteMsg:
		m.platformInfo = msg.Info
		m.done = true
		return m, tea.Quit

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m PlatformStartupModel) View() string {
	if m.quitting {
		return ""
	}

	if m.done {
		return ""
	}

	var s strings.Builder

	// Title
	s.WriteString(FormatTitle(EmojiRocket, "Initializing Meko intelligence platform..."))
	s.WriteString("\n\n")

	// Show all steps with their status
	for i, step := range m.steps {
		var statusIcon string
		var statusStyle lipgloss.Style

		switch step.Status {
		case "pending":
			statusIcon = "○"
			statusStyle = lipgloss.NewStyle().Foreground(ColorMuted)
		case "running":
			statusIcon = m.spinner.View()
			statusStyle = lipgloss.NewStyle().Foreground(ColorInfo)
		case "success":
			statusIcon = EmojiCheckmark
			statusStyle = lipgloss.NewStyle().Foreground(ColorSuccess)
		case "error":
			statusIcon = EmojiCross
			statusStyle = lipgloss.NewStyle().Foreground(ColorError)
		}

		stepText := fmt.Sprintf("%s %s", statusIcon, step.Description)
		s.WriteString(statusStyle.Render(stepText))
		s.WriteString("\n")

		// Show details for completed steps
		if step.Status == "success" && len(step.Details) > 0 {
			for _, detail := range step.Details {
				s.WriteString(MutedText.Render("  • " + detail))
				s.WriteString("\n")
			}
		}

		// Show error if any
		if step.Status == "error" && step.Error != nil {
			s.WriteString(ErrorText.Render("  ✗ " + step.Error.Error()))
			s.WriteString("\n")
		}

		// Add spacing between steps
		if i < len(m.steps)-1 {
			s.WriteString("\n")
		}
	}

	return BoxStyle.Render(s.String())
}

// GetError returns any error that occurred
func (m PlatformStartupModel) GetError() error {
	return m.err
}

// GetPlatformInfo returns the platform info
func (m PlatformStartupModel) GetPlatformInfo() PlatformInfo {
	return m.platformInfo
}
