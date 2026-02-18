package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ViewMode represents different screens in the TUI
type ViewMode int

const (
	ViewLanding ViewMode = iota
	ViewMenu
	ViewDetail
	ViewLoading
	ViewInput
)

// Model represents the main TUI model
type Model struct {
	list               list.Model
	commandList        list.Model
	textarea           textarea.Model
	err                error
	content            string
	title              string
	keys               keyMap
	viewport           viewport.Model
	spinner            spinner.Model
	width              int
	mode               ViewMode
	height             int
	pulseOffset        float64
	ready              bool
	quitting           bool
	showCommandPalette bool
	commandListFocused bool
}

// keyMap defines keyboard shortcuts
type keyMap struct {
	Up      key.Binding
	Down    key.Binding
	Left    key.Binding
	Right   key.Binding
	Enter   key.Binding
	Back    key.Binding
	Quit    key.Binding
	Help    key.Binding
	Refresh key.Binding
}

// Styles
var (
	special = lipgloss.AdaptiveColor{Light: "#43BF6D", Dark: "#73F59F"}

	titleStyle = lipgloss.NewStyle().
			Foreground(special).
			Bold(true).
			Padding(0, 1)

	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#3C3C5C")).
			Padding(0, 1)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#626262"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF5555")).
			Bold(true)

	contentStyle = lipgloss.NewStyle().
			Padding(1, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#7D56F4"))

	focusedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7D56F4")).
			Bold(true)

	selectedItemStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#7D56F4")).
				Bold(true).
				PaddingLeft(2)
)

// NewKeyMap returns the default key mappings
func NewKeyMap() keyMap {
	return keyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "move up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "move down"),
		),
		Left: key.NewBinding(
			key.WithKeys("left", "h"),
			key.WithHelp("←/h", "move left"),
		),
		Right: key.NewBinding(
			key.WithKeys("right", "l"),
			key.WithHelp("→/l", "move right"),
		),
		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "select"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc", "backspace"),
			key.WithHelp("esc", "go back"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "toggle help"),
		),
		Refresh: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "refresh"),
		),
	}
}

// Item represents a list item
type Item struct {
	title       string
	description string
}

func (i Item) Title() string       { return i.title }
func (i Item) Description() string { return i.description }
func (i Item) FilterValue() string { return i.title }

// Command represents a command palette item
type Command struct {
	name        string
	description string
	action      string
}

func (c Command) Title() string       { return c.name }
func (c Command) Description() string { return c.description }
func (c Command) FilterValue() string { return c.name }

// getAvailableCommands returns all available commands for the command palette
func getAvailableCommands() []list.Item {
	cfg := GetTextConfig()
	commands := make([]list.Item, 0, len(cfg.CommandPalette.Commands))

	for _, cmd := range cfg.CommandPalette.Commands {
		commands = append(commands, Command{
			name:        cmd.Name,
			description: cmd.Description,
			action:      cmd.Action,
		})
	}

	return commands
}

// NewModel creates a new TUI model
func NewModel() Model {
	// Load text configuration
	cfg := GetTextConfig()

	// Create list items from config
	items := make([]list.Item, 0, len(cfg.Menu.Items))
	for _, item := range cfg.Menu.Items {
		items = append(items, Item{
			title:       item.Title,
			description: item.Description,
		})
	}

	// Initialize list
	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = selectedItemStyle
	delegate.Styles.SelectedDesc = selectedItemStyle

	l := list.New(items, delegate, 0, 0)
	l.Title = cfg.Menu.Title
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(true)
	l.Styles.Title = titleStyle

	// Initialize command list for command palette
	commandDelegate := list.NewDefaultDelegate()
	commandDelegate.Styles.SelectedTitle = selectedItemStyle
	commandDelegate.Styles.SelectedDesc = selectedItemStyle

	commandList := list.New(getAvailableCommands(), commandDelegate, 0, 0)
	commandList.Title = cfg.CommandPalette.Title
	commandList.SetShowStatusBar(false)
	commandList.SetFilteringEnabled(true)
	commandList.Styles.Title = titleStyle

	// Initialize spinner
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(cfg.Styles.Colors.Primary))

	// Initialize textarea
	ta := textarea.New()
	ta.Placeholder = cfg.Messages.TextareaPlaceholder
	ta.Focus()
	ta.CharLimit = 500

	return Model{
		mode:               ViewLanding,
		list:               l,
		commandList:        commandList,
		spinner:            s,
		textarea:           ta,
		keys:               NewKeyMap(),
		showCommandPalette: false,
		commandListFocused: false,
	}
}

// pulseTickMsg is sent periodically to update the pulse animation
type pulseTickMsg time.Time

// parseHexColor parses a hex color string to RGB values
func parseHexColor(hexColor string) struct{ r, g, b int } {
	// Remove # if present
	hexColor = strings.TrimPrefix(hexColor, "#")

	// Parse RGB values
	var r, g, b int
	if len(hexColor) == 6 {
		if _, err := fmt.Sscanf(hexColor, "%02x%02x%02x", &r, &g, &b); err != nil {
			return struct{ r, g, b int }{0, 0, 0}
		}
	}

	return struct{ r, g, b int }{r, g, b}
}

// tickPulse returns a command that sends a pulse tick message
func tickPulse() tea.Cmd {
	return tea.Tick(time.Millisecond*50, func(t time.Time) tea.Msg {
		return pulseTickMsg(t)
	})
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		tea.EnterAltScreen,
		tickPulse(),
	)
}

// Update handles messages and updates the model
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		if !m.ready {
			m.viewport = viewport.New(msg.Width, msg.Height-5)
			m.viewport.YPosition = 0
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = msg.Height - 5
		}

		m.list.SetSize(msg.Width, msg.Height-2)
		m.commandList.SetSize(msg.Width-4, 15)

		// Update textarea width for responsive resizing
		m.textarea.SetWidth(msg.Width - 20)

		// Force a re-render by returning the model
		// The View() function will be called automatically with the new dimensions
		return m, nil

	case tea.KeyMsg:
		if m.quitting {
			return m, tea.Quit
		}

		// Handle command palette navigation
		if m.showCommandPalette && m.commandListFocused {
			switch msg.String() {
			case "esc":
				m.showCommandPalette = false
				m.commandListFocused = false
				m.textarea.Focus()
				return m, nil
			case "enter":
				// Execute selected command
				selected := m.commandList.SelectedItem()
				if cmd, ok := selected.(Command); ok {
					m.showCommandPalette = false
					m.commandListFocused = false
					m.executeCommand(cmd.action)
					return m, nil
				}
			case "up", "down", "j", "k":
				m.commandList, cmd = m.commandList.Update(msg)
				return m, cmd
			}
		}

		switch {
		case key.Matches(msg, m.keys.Quit):
			m.quitting = true
			return m, tea.Quit

		case key.Matches(msg, m.keys.Back):
			if m.mode == ViewMenu {
				m.mode = ViewLanding
				return m, nil
			} else if m.mode != ViewLanding {
				m.mode = ViewMenu
				return m, nil
			}

		case key.Matches(msg, m.keys.Enter):
			if m.mode == ViewLanding {
				// Check if command palette is showing
				if m.showCommandPalette {
					m.commandListFocused = true
					return m, nil
				}
				// Transition from landing to menu
				m.mode = ViewMenu
				return m, nil
			}
			if m.mode == ViewMenu {
				i, ok := m.list.SelectedItem().(Item)
				if ok {
					m.handleSelection(i.title)
					return m, nil
				}
			}
		}

	case spinner.TickMsg:
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case pulseTickMsg:
		// Update pulse offset for breathing effect
		m.pulseOffset += 0.1
		if m.pulseOffset > 2*math.Pi {
			m.pulseOffset = 0
		}
		return m, tickPulse()
	}

	// Update the appropriate component based on mode
	switch m.mode {
	case ViewLanding:
		m.textarea, cmd = m.textarea.Update(msg)
		cmds = append(cmds, cmd)

		// Check if user typed "/" to show command palette
		text := m.textarea.Value()
		if strings.HasPrefix(text, "/") && !m.showCommandPalette {
			m.showCommandPalette = true
			// Filter command list based on typed text
			m.commandList.ResetFilter()
		} else if !strings.HasPrefix(text, "/") && m.showCommandPalette {
			m.showCommandPalette = false
			m.commandListFocused = false
		}

	case ViewMenu:
		m.list, cmd = m.list.Update(msg)
		cmds = append(cmds, cmd)
	case ViewDetail:
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	case ViewInput:
		m.textarea, cmd = m.textarea.Update(msg)
		cmds = append(cmds, cmd)

		// Check if user typed "/" to show command palette
		text := m.textarea.Value()
		if strings.HasPrefix(text, "/") && !m.showCommandPalette {
			m.showCommandPalette = true
			m.commandList.ResetFilter()
		} else if !strings.HasPrefix(text, "/") && m.showCommandPalette {
			m.showCommandPalette = false
			m.commandListFocused = false
		}

	case ViewLoading:
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// View renders the UI
func (m Model) View() string {
	cfg := GetTextConfig()

	if m.quitting {
		return cfg.Messages.Quit + "\n"
	}

	if !m.ready {
		return "\n  " + cfg.Messages.Initializing
	}

	var content string

	switch m.mode {
	case ViewLanding:
		content = m.renderLandingView()

	case ViewMenu:
		content = m.list.View()

	case ViewDetail:
		content = m.renderDetailView()

	case ViewInput:
		content = m.renderInputView()

	case ViewLoading:
		content = m.renderLoadingView()
	}

	// Add error message if present
	if m.err != nil {
		errMsg := errorStyle.Render(fmt.Sprintf("\n⚠ Error: %v", m.err))
		content = lipgloss.JoinVertical(lipgloss.Left, content, errMsg)
	}

	return content
}

// handleSelection processes menu selections
func (m *Model) handleSelection(item string) {
	switch item {
	case "Dashboard":
		m.mode = ViewDetail
		m.title = "Dashboard"
		m.content = m.generateDashboardContent()
		m.viewport.SetContent(m.content)

	case "Database":
		m.mode = ViewDetail
		m.title = "Database Management"
		m.content = m.generateDatabaseContent()
		m.viewport.SetContent(m.content)

	case "Agent":
		m.mode = ViewInput
		m.title = "AI Agent"

	case "Configuration":
		m.mode = ViewDetail
		m.title = "Configuration"
		m.content = m.generateConfigContent()
		m.viewport.SetContent(m.content)

	case "Logs":
		m.mode = ViewDetail
		m.title = "Application Logs"
		m.content = m.generateLogsContent()
		m.viewport.SetContent(m.content)

	case "Help":
		m.mode = ViewDetail
		m.title = "Help"
		m.content = m.generateHelpContent()
		m.viewport.SetContent(m.content)
	}
}

// executeCommand executes a command from the command palette
func (m *Model) executeCommand(action string) {
	switch action {
	case "dashboard":
		m.mode = ViewDetail
		m.title = "Dashboard"
		m.content = m.generateDashboardContent()
		m.viewport.SetContent(m.content)
		m.textarea.SetValue("")

	case "database":
		m.mode = ViewDetail
		m.title = "Database Management"
		m.content = m.generateDatabaseContent()
		m.viewport.SetContent(m.content)
		m.textarea.SetValue("")

	case "agent":
		m.mode = ViewInput
		m.title = "AI Agent"
		m.textarea.SetValue("")

	case "config":
		m.mode = ViewDetail
		m.title = "Configuration"
		m.content = m.generateConfigContent()
		m.viewport.SetContent(m.content)
		m.textarea.SetValue("")

	case "logs":
		m.mode = ViewDetail
		m.title = "Application Logs"
		m.content = m.generateLogsContent()
		m.viewport.SetContent(m.content)
		m.textarea.SetValue("")

	case "help":
		m.mode = ViewDetail
		m.title = "Help"
		m.content = m.generateHelpContent()
		m.viewport.SetContent(m.content)
		m.textarea.SetValue("")

	case "clear":
		m.textarea.SetValue("")

	case "quit":
		m.quitting = true
	}
}

// renderDetailView renders the detail view
func (m Model) renderDetailView() string {
	header := titleStyle.Render(m.title)
	statusBar := m.renderStatusBar()

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		"",
		m.viewport.View(),
		"",
		statusBar,
	)

	return content
}

// renderInputView renders the input view
func (m Model) renderInputView() string {
	cfg := GetTextConfig()
	header := titleStyle.Render(m.title)
	statusBar := m.renderStatusBar()

	textareaView := contentStyle.Render(m.textarea.View())

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		"",
		textareaView,
		"",
		helpStyle.Render(cfg.Messages.InputHelp),
		"",
		statusBar,
	)

	// Show command palette if active
	if m.showCommandPalette {
		commandPalette := m.renderCommandPalette()
		content = lipgloss.JoinVertical(
			lipgloss.Left,
			content,
			"\n",
			commandPalette,
		)
	}

	return content
}

// renderLoadingView renders the loading view
func (m Model) renderLoadingView() string {
	cfg := GetTextConfig()
	return fmt.Sprintf("\n\n   %s %s\n\n", m.spinner.View(), cfg.Messages.Loading)
}

// renderLandingView renders the landing page with ASCII art and text area
func (m Model) renderLandingView() string {
	cfg := GetTextConfig()
	asciiLines := cfg.Logo.Lines

	// Base color from config
	baseColor := parseHexColor(cfg.Logo.Color)

	var hexColor string
	if cfg.Logo.Pulsing {
		// Calculate pulse brightness using sine wave (0.5 to 1.0)
		// sin(t) ranges from -1 to 1, we map it to 0.5 to 1.0 for breathing effect
		pulseBrightness := 0.75 + 0.25*math.Sin(m.pulseOffset)

		// Modulate brightness
		r := int(float64(baseColor.r) * pulseBrightness)
		g := int(float64(baseColor.g) * pulseBrightness)
		b := int(float64(baseColor.b) * pulseBrightness)

		// Clamp values
		if r > 255 {
			r = 255
		}
		if g > 255 {
			g = 255
		}
		if b > 255 {
			b = 255
		}
		if r < 0 {
			r = 0
		}
		if g < 0 {
			g = 0
		}
		if b < 0 {
			b = 0
		}

		hexColor = fmt.Sprintf("#%02X%02X%02X", r, g, b)
	} else {
		hexColor = cfg.Logo.Color
	}

	// Apply pulse effect to each line with the same color
	styledLines := make([]string, 0, len(asciiLines))
	for _, line := range asciiLines {
		styledLine := lipgloss.NewStyle().
			Foreground(lipgloss.Color(hexColor)).
			Bold(cfg.Logo.Bold).
			Render(line)
		styledLines = append(styledLines, styledLine)
	}

	styledASCII := strings.Join(styledLines, "\n")

	// Style the textarea with responsive width
	textareaWidth := m.width - 8
	// Ensure minimum and maximum widths
	if textareaWidth < 40 {
		textareaWidth = 40
	}
	if textareaWidth > 120 {
		textareaWidth = 120
	}

	textareaView := lipgloss.NewStyle().
		Width(textareaWidth).
		Padding(0, 2).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(cfg.Styles.Colors.Border)).
		Render(m.textarea.View())

	// Help text - changes based on command palette state
	var helpText string
	if m.showCommandPalette {
		if m.commandListFocused {
			helpText = helpStyle.Render(cfg.Messages.LandingPaletteFocused)
		} else {
			helpText = helpStyle.Render(cfg.Messages.LandingPaletteVisible)
		}
	} else {
		helpText = helpStyle.Render(cfg.Messages.LandingNormal)
	}

	// Welcome message - larger text using styling
	// Calculate responsive width for welcome section (max 60, but adjust for smaller screens)
	welcomeWidth := 60
	if m.width < 120 {
		welcomeWidth = m.width / 3
		if welcomeWidth < 30 {
			welcomeWidth = 30
		}
	}

	welcomeMsg := lipgloss.NewStyle().
		Bold(cfg.Welcome.Bold).
		Foreground(lipgloss.Color(cfg.Welcome.TitleColor)).
		Width(welcomeWidth).
		Align(lipgloss.Left).
		PaddingLeft(2).
		Render(cfg.Welcome.Title)

	// Help text with larger size
	welcomeHelp := lipgloss.NewStyle().
		Foreground(lipgloss.Color(cfg.Welcome.TextColor)).
		Bold(cfg.Welcome.Bold).
		Width(welcomeWidth).
		Align(lipgloss.Left).
		PaddingLeft(2).
		Render("\n" + cfg.Welcome.Help)

	welcomeHint := lipgloss.NewStyle().
		Foreground(lipgloss.Color(cfg.Welcome.TextColor)).
		Bold(cfg.Welcome.Bold).
		Width(welcomeWidth).
		Align(lipgloss.Left).
		PaddingLeft(2).
		Render("\n" + cfg.Welcome.Hint)

	welcomeSection := lipgloss.JoinVertical(
		lipgloss.Left,
		welcomeMsg,
		welcomeHelp,
		welcomeHint,
	)

	// Join ASCII art and welcome message
	// Use horizontal layout for wider screens, vertical for narrow screens
	var combinedSection string
	minWidthForHorizontal := 100

	if m.width >= minWidthForHorizontal {
		// Horizontal layout for wider screens
		combinedSection = lipgloss.JoinHorizontal(
			lipgloss.Center,
			styledASCII,
			"      ", // Spacing between sections
			welcomeSection,
		)
	} else {
		// Vertical layout for narrow screens
		combinedSection = lipgloss.JoinVertical(
			lipgloss.Center,
			styledASCII,
			"\n",
			welcomeSection,
		)
	}

	// Center the combined section
	centeredCombined := lipgloss.Place(
		m.width, lipgloss.Height(combinedSection),
		lipgloss.Center, lipgloss.Center,
		combinedSection,
	)

	centeredTextarea := lipgloss.Place(
		m.width, lipgloss.Height(textareaView),
		lipgloss.Center, lipgloss.Center,
		textareaView,
	)

	centeredHelp := lipgloss.Place(
		m.width, lipgloss.Height(helpText),
		lipgloss.Center, lipgloss.Center,
		helpText,
	)

	// Combine all elements
	var content string
	if m.showCommandPalette {
		// Show command palette below textarea
		commandPalette := m.renderCommandPalette()
		// Calculate available height for command palette
		commandPaletteHeight := 18 // approximate height
		availableHeight := m.height
		usedHeight := commandPaletteHeight + lipgloss.Height(centeredCombined) +
			lipgloss.Height(centeredTextarea) + lipgloss.Height(centeredHelp)
		topPadding := (availableHeight - usedHeight) / 2
		if topPadding < 1 {
			topPadding = 1
		}
		padding := strings.Repeat("\n", topPadding)

		content = lipgloss.JoinVertical(
			lipgloss.Center,
			padding,
			centeredCombined,
			"\n\n",
			centeredTextarea,
			"\n",
			commandPalette,
			"\n",
			centeredHelp,
		)
	} else {
		// Calculate vertical padding to center the ASCII art
		asciiLineCount := len(asciiLines)
		textareaHeight := 5
		helpHeight := 2
		totalContentHeight := asciiLineCount + textareaHeight + helpHeight + 6 // +6 for spacing
		topPadding := (m.height - totalContentHeight) / 2
		if topPadding < 0 {
			topPadding = 1
		}
		padding := strings.Repeat("\n", topPadding)

		content = lipgloss.JoinVertical(
			lipgloss.Center,
			padding,
			centeredCombined,
			"\n\n",
			centeredTextarea,
			"\n",
			centeredHelp,
		)
	}

	// Ensure content fills the entire screen height to prevent artifacts
	contentHeight := lipgloss.Height(content)
	if contentHeight < m.height {
		bottomPadding := strings.Repeat("\n", m.height-contentHeight)
		content += bottomPadding
	}

	return content
}

// renderStatusBar renders the bottom status bar
func (m Model) renderStatusBar() string {
	cfg := GetTextConfig()
	var helpText string
	if m.showCommandPalette {
		helpText = cfg.Messages.StatusPalette
	} else {
		helpText = cfg.Messages.StatusHelp
	}
	return statusBarStyle.Render(helpText)
}

// renderCommandPalette renders the command palette overlay
func (m Model) renderCommandPalette() string {
	// Ensure minimum width
	paletteWidth := m.width - 20
	if paletteWidth < 40 {
		paletteWidth = 40
	}
	if paletteWidth > 60 {
		paletteWidth = 60
	}

	commandPaletteStyle := lipgloss.NewStyle().
		Width(paletteWidth).
		Padding(1, 2).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7D56F4")).
		Background(lipgloss.Color("#1E1E2E"))

	paletteContent := commandPaletteStyle.Render(m.commandList.View())

	centered := lipgloss.Place(
		m.width, lipgloss.Height(paletteContent),
		lipgloss.Center, lipgloss.Center,
		paletteContent,
	)

	return centered
}

// Content generation methods
func (m Model) generateDashboardContent() string {
	cfg := GetTextConfig()
	return cfg.Content.Dashboard.Format()
}

func (m Model) generateDatabaseContent() string {
	cfg := GetTextConfig()
	return cfg.Content.Database.Format()
}

func (m Model) generateConfigContent() string {
	cfg := GetTextConfig()
	return cfg.Content.Config.Format()
}

func (m Model) generateLogsContent() string {
	cfg := GetTextConfig()
	return cfg.Content.Logs.Format()
}

func (m Model) generateHelpContent() string {
	cfg := GetTextConfig()
	return cfg.Content.Help.Format()
}
