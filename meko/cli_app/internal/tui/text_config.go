package tui

import (
	"embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed text_config.yaml
var configFile embed.FS

// TextConfig holds all text content and styling configuration
type TextConfig struct {
	Messages struct {
		Quit                  string `yaml:"quit"`
		Initializing          string `yaml:"initializing"`
		Loading               string `yaml:"loading"`
		LandingNormal         string `yaml:"landing_normal"`
		LandingPaletteVisible string `yaml:"landing_palette_visible"`
		LandingPaletteFocused string `yaml:"landing_palette_focused"`
		InputHelp             string `yaml:"input_help"`
		StatusHelp            string `yaml:"status_help"`
		StatusPalette         string `yaml:"status_palette"`
		TextareaPlaceholder   string `yaml:"textarea_placeholder"`
	} `yaml:"messages"`
	Content struct {
		Dashboard ContentSection `yaml:"dashboard"`
		Database  ContentSection `yaml:"database"`
		Config    ContentSection `yaml:"config"`
		Logs      ContentSection `yaml:"logs"`
		Help      ContentSection `yaml:"help"`
		Agent     ContentSection `yaml:"agent"`
	} `yaml:"content"`
	Styles struct {
		Colors struct {
			Primary    string `yaml:"primary"`
			Secondary  string `yaml:"secondary"`
			Error      string `yaml:"error"`
			Muted      string `yaml:"muted"`
			Background string `yaml:"background"`
			Border     string `yaml:"border"`
		} `yaml:"colors"`
		Emphasis struct {
			Bold      bool `yaml:"bold"`
			Underline bool `yaml:"underline"`
			Italic    bool `yaml:"italic"`
		} `yaml:"emphasis"`
	} `yaml:"styles"`
	Welcome struct {
		Title      string `yaml:"title"`
		Help       string `yaml:"help"`
		Hint       string `yaml:"hint"`
		TitleColor string `yaml:"title_color"`
		TextColor  string `yaml:"text_color"`
		Bold       bool   `yaml:"bold"`
	} `yaml:"welcome"`
	Menu struct {
		Title string `yaml:"title"`
		Items []struct {
			Title       string `yaml:"title"`
			Description string `yaml:"description"`
		} `yaml:"items"`
	} `yaml:"menu"`
	CommandPalette struct {
		Title    string `yaml:"title"`
		Commands []struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
			Action      string `yaml:"action"`
		} `yaml:"commands"`
	} `yaml:"command_palette"`
	Logo struct {
		Color   string   `yaml:"color"`
		Lines   []string `yaml:"lines"`
		Pulsing bool     `yaml:"pulsing"`
		Bold    bool     `yaml:"bold"`
	} `yaml:"logo"`
}

// ContentSection represents a content area with title and sections
type ContentSection struct {
	Title    string `yaml:"title"`
	Sections []struct {
		Title string   `yaml:"title"`
		Items []string `yaml:"items"`
	} `yaml:"sections"`
}

// Global text config instance
var textConfig *TextConfig

// LoadTextConfig loads the text configuration from the embedded YAML file
func LoadTextConfig() (*TextConfig, error) {
	if textConfig != nil {
		return textConfig, nil
	}

	data, err := configFile.ReadFile("text_config.yaml")
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config TextConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	textConfig = &config
	return textConfig, nil
}

// GetTextConfig returns the loaded text config or loads it if not already loaded
func GetTextConfig() *TextConfig {
	if textConfig == nil {
		config, err := LoadTextConfig()
		if err != nil {
			// Return a default config if loading fails
			return getDefaultTextConfig()
		}
		return config
	}
	return textConfig
}

// Helper method to format content sections
func (cs *ContentSection) Format() string {
	var b strings.Builder

	for _, section := range cs.Sections {
		if section.Title != "" {
			b.WriteString(focusedStyle.Render(section.Title))
			b.WriteString("\n\n")
		}
		for _, item := range section.Items {
			b.WriteString(item)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	return strings.TrimSpace(b.String())
}

// getDefaultTextConfig returns a fallback configuration
func getDefaultTextConfig() *TextConfig {
	return &TextConfig{
		Logo: struct {
			Color   string   `yaml:"color"`
			Lines   []string `yaml:"lines"`
			Pulsing bool     `yaml:"pulsing"`
			Bold    bool     `yaml:"bold"`
		}{
			Lines: []string{
				"██╗   ██╗██╗   ██╗ ██████╗  █████╗     █████╗ ██╗",
				"╚██╗ ██╔╝██║   ██║██╔════╝ ██╔══██╗   ██╔══██╗██║",
				" ╚████╔╝ ██║   ██║██║  ███╗███████║   ███████║██║",
				"  ╚██╔╝  ██║   ██║██║   ██║██╔══██║   ██╔══██║██║",
				"   ██║   ╚██████╔╝╚██████╔╝██║  ██║██╗██║  ██║██║",
				"   ╚═╝    ╚═════╝  ╚═════╝ ╚═╝  ╚═╝╚═╝╚═╝  ╚═╝╚═╝",
			},
			Color:   "#FF6F00",
			Pulsing: true,
			Bold:    true,
		},
		Messages: struct {
			Quit                  string `yaml:"quit"`
			Initializing          string `yaml:"initializing"`
			Loading               string `yaml:"loading"`
			LandingNormal         string `yaml:"landing_normal"`
			LandingPaletteVisible string `yaml:"landing_palette_visible"`
			LandingPaletteFocused string `yaml:"landing_palette_focused"`
			InputHelp             string `yaml:"input_help"`
			StatusHelp            string `yaml:"status_help"`
			StatusPalette         string `yaml:"status_palette"`
			TextareaPlaceholder   string `yaml:"textarea_placeholder"`
		}{
			Quit:                  "Thanks for using Yuga CLI!",
			Initializing:          "Initializing...",
			Loading:               "Loading...",
			LandingNormal:         "Press Enter to continue • Type / for commands • q to quit",
			LandingPaletteVisible: "Press Enter to select command • Esc to cancel • q to quit",
			LandingPaletteFocused: "↑↓ to navigate • Enter to select • Esc to cancel",
			InputHelp:             "Press Enter to submit, Esc to go back",
			StatusHelp:            "Press ? for help • q to quit • esc to go back",
			StatusPalette:         "Use ↑↓ to navigate • Enter to select • Esc to cancel",
			TextareaPlaceholder:   "Type your message here... (/ for commands)",
		},
	}
}
