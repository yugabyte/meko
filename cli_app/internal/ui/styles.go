package ui

import (
	"github.com/charmbracelet/lipgloss"
)

// Common color palette
var (
	ColorPrimary = lipgloss.Color("#7D56F4")
	ColorSuccess = lipgloss.Color("#04B575")
	ColorError   = lipgloss.Color("#FF0000")
	ColorWarning = lipgloss.Color("#FFA500")
	ColorInfo    = lipgloss.Color("#00A9FF")
	ColorMuted   = lipgloss.Color("#888888")
)

// Common styles for consistent UI across all commands
var (
	// TitleStyle is for main headings
	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorPrimary).
			Padding(1, 2)

	// SuccessStyle is for success messages
	SuccessStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorSuccess).
			Padding(1, 2)

	// ErrorStyle is for error messages
	ErrorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorError).
			Padding(1, 2)

	// WarningStyle is for warning messages
	WarningStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorWarning).
			Padding(1, 2)

	// InfoStyle is for informational messages
	InfoStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorInfo).
			Padding(1, 2)

	// LogStyle is for log/output text
	LogStyle = lipgloss.NewStyle().
			Foreground(ColorMuted).
			PaddingLeft(2)

	// BoxStyle is for rounded border boxes
	BoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorPrimary).
			Padding(1, 2)

	// SubtleBoxStyle is a more subtle box style
	SubtleBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorMuted).
			Padding(1, 2)

	// Inline styles (without padding) for use within text
	SuccessText = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorSuccess)

	ErrorText = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorError)

	WarningText = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorWarning)

	InfoText = lipgloss.NewStyle().
			Foreground(ColorInfo)

	MutedText = lipgloss.NewStyle().
			Foreground(ColorMuted)
)

// Emojis for consistent icons
const (
	EmojiRocket    = "🚀"
	EmojiCheckmark = "✅"
	EmojiCross     = "❌"
	EmojiWarning   = "⚠️"
	EmojiInfo      = "ℹ️"
	EmojiParty     = "🎉"
	EmojiDatabase  = "💾"
	EmojiGear      = "⚙️"
	EmojiChart     = "📊"
)

// DividerLine returns a horizontal divider line of specified length
func DividerLine(length int) string {
	if length <= 0 {
		length = 80
	}
	line := ""
	for i := 0; i < length; i++ {
		line += "─"
	}
	return line
}

// FormatTitle formats a title with emoji and style
func FormatTitle(emoji, text string) string {
	return TitleStyle.Render(emoji + " " + text)
}

// FormatSuccess formats a success message with emoji and style
func FormatSuccess(text string) string {
	return SuccessStyle.Render(EmojiCheckmark + " " + text)
}

// FormatError formats an error message with emoji and style
func FormatError(text string) string {
	return ErrorStyle.Render(EmojiCross + " " + text)
}

// FormatWarning formats a warning message with emoji and style
func FormatWarning(text string) string {
	return WarningStyle.Render(EmojiWarning + " " + text)
}

// FormatInfo formats an info message with emoji and style
func FormatInfo(text string) string {
	return InfoStyle.Render(EmojiInfo + " " + text)
}
