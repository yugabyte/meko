# meko CLI

A powerful Terminal User Interface (TUI) CLI tool for the meko Platform, built with [Cobra](https://github.com/spf13/cobra) and [Bubble Tea](https://github.com/charmbracelet/bubbletea).

Inspired by Sourcegraph's AMP CLI, this tool provides a modern, interactive command-line experience with beautiful live updates and intuitive navigation.

## Features

- 🎨 **Beautiful TUI** - Modern, colorful interface with smooth animations
- ⚡ **Live Updates** - Real-time status monitoring and updates
- 🎯 **Interactive Navigation** - Keyboard-driven menu system
- 📊 **Multiple Views** - Dashboard, database management, logs, and more
- 🔧 **Traditional CLI** - Both interactive TUI and standard CLI commands
- 🎭 **Responsive** - Adapts to terminal size changes

## Installation

### Prerequisites

- Go 1.21 or higher
- YugabyteDB instance (for database features)

### Build from source

```bash
cd cli_app
go mod download
go build -o meko .
```

### Install globally

```bash
go install
```

## Quick Start

### Interactive TUI Mode

Launch the full interactive TUI experience:

```bash
./meko interactive
```

Or use shortcuts:

```bash
./meko i      # Short form
./meko tui    # Alias
./meko ui     # Another alias
```

### Traditional CLI Commands

Check system status:

```bash
./meko status
```

Database commands:

```bash
./meko db status                    # Check database status
./meko db connect                   # Connect to database
./meko db status --host localhost --port 5433
```

Get help:

```bash
./meko --help
./meko db --help
```

## TUI Navigation

### Keyboard Shortcuts

- **↑/k** or **↓/j** - Navigate up/down
- **←/h** or **→/l** - Navigate left/right (context-dependent)
- **Enter** - Select/confirm
- **Esc** - Go back to previous screen
- **q** - Quit application
- **r** - Refresh content
- **?** - Toggle help
- **/** - Filter/search (in list views)

### Available Views

1. **Dashboard** - System overview with metrics and status
2. **Database** - YugabyteDB management and monitoring
3. **Agent** - AI Agent interaction interface
4. **Configuration** - View and edit settings
5. **Logs** - Application logs viewer
6. **Help** - Documentation and keyboard shortcuts

## Project Structure

```
cli_app/
├── main.go                    # Application entry point
├── go.mod                     # Go module dependencies
├── cmd/                       # Cobra commands
│   ├── root.go               # Root command and config
│   ├── interactive.go        # TUI mode command
│   ├── status.go             # Status command
│   └── db.go                 # Database commands
├── internal/
│   ├── tui/                  # Bubble Tea TUI components
│   │   └── model.go          # Main TUI model
│   └── models/               # Data models (optional)
└── README.md                 # This file
```

## Architecture

### Cobra Commands

The CLI uses [Cobra](https://github.com/spf13/cobra) for command structure:

- **Root command** (`meko`) - Entry point with global flags
- **Interactive command** - Launches the TUI
- **Traditional commands** - Standard CLI operations (status, db, etc.)

### Bubble Tea TUI

The interactive mode uses [Bubble Tea](https://github.com/charmbracelet/bubbletea) for the TUI:

- **Model** - Application state and business logic
- **Update** - Message handling and state transitions
- **View** - Rendering logic for different screens

### Components Used

- **Bubbles** - Pre-built components (list, viewport, spinner, textarea)
- **Lipgloss** - Styling and layout
- **Multiple view modes** - Menu, detail, input, loading screens

## Development

### Adding New Commands

Create a new file in `cmd/`:

```go
package cmd

import (
    "fmt"
    "github.com/spf13/cobra"
)

var myCmd = &cobra.Command{
    Use:   "mycommand",
    Short: "Description",
    RunE: func(cmd *cobra.Command, args []string) error {
        fmt.Println("Hello from my command!")
        return nil
    },
}

func init() {
    rootCmd.AddCommand(myCmd)
}
```

### Adding New TUI Views

Extend the `Model` in `internal/tui/model.go`:

1. Add a new `ViewMode` constant
2. Implement the view rendering logic
3. Add navigation handling in `handleSelection()`
4. Create a content generation method

### Customizing Styles

Modify the `lipgloss` styles in `internal/tui/model.go`:

```go
var myStyle = lipgloss.NewStyle().
    Foreground(lipgloss.Color("#7D56F4")).
    Background(lipgloss.Color("#1E1E2E")).
    Padding(1, 2)
```

## Dependencies

- **github.com/spf13/cobra** - CLI framework
- **github.com/charmbracelet/bubbletea** - TUI framework
- **github.com/charmbracelet/bubbles** - TUI components
- **github.com/charmbracelet/lipgloss** - Styling

## Configuration

Configuration can be loaded from:

- Default: `$HOME/.meko.yaml`
- Custom: Use `--config` flag

Example config:

```yaml
database:
  host: localhost
  port: 5433
  name: yugabyte

agent:
  model: gpt-4
  temperature: 0.7

ui:
  theme: dark
```

## Examples

### Launch interactive mode and navigate to database view

```bash
./meko interactive
# Then: Use arrow keys to select "Database" and press Enter
```

### Check status with verbose output

```bash
./meko status --verbose
```

### Connect to remote database

```bash
./meko db connect --host 192.168.1.100 --port 5433
```

## Troubleshooting

### TUI not rendering correctly

- Ensure your terminal supports colors (most modern terminals do)
- Try a different terminal emulator
- Check terminal size (minimum 80x24 recommended)

### Commands not found

- Ensure Go bin is in your PATH
- Rebuild the binary: `go build -o meko .`

### Dependencies issues

```bash
go mod tidy
go mod download
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Test thoroughly
5. Submit a pull request

## License

MIT License - see LICENSE file for details

## Acknowledgments

- [Cobra](https://github.com/spf13/cobra) - CLI framework
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) - TUI framework
- [Charm](https://charm.sh/) - Amazing terminal tools
- Inspired by [Sourcegraph AMP CLI](https://sourcegraph.com/)

## Resources

- [Bubble Tea Documentation](https://github.com/charmbracelet/bubbletea)
- [Cobra Documentation](https://cobra.dev/)
- [Lipgloss Examples](https://github.com/charmbracelet/lipgloss/tree/master/examples)
- [Bubble Tea Examples](https://github.com/charmbracelet/bubbletea/tree/master/examples)

## Support

For issues and questions:
- Open an issue on GitHub
- Check existing documentation
- Review example implementations

---

**Enjoy building beautiful CLI applications with Yuga CLI!** 🚀
