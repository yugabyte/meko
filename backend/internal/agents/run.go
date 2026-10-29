package agents

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CLITimeout bounds one run of an agent's CLI.
const CLITimeout = 60 * time.Second

// lookPath is exec.LookPath's result as an absolute path.
func lookPath(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// runCLI runs an agent's CLI (cmd, the absolute path Find gave) in the folder dir ("" for this
// one), with no stdin, so one that would ask asks no one. Its folder goes first on PATH: an npm
// install's command finds its Node there, which a service's thin PATH lacks. One in a folder of the
// login shell's PATH gets that PATH next, before Meko's own. Its error has the output's last line.
func runCLI(ctx context.Context, dir, cmd string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, CLITimeout)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...) //nolint:gosec // G204: the agent's own CLI, found by Find
	c.Dir = dir
	path := append(append([]string{filepath.Dir(cmd)}, loginPathOf(cmd)...), os.Getenv("PATH"))
	c.Env = append(os.Environ(), "PATH="+strings.Join(path, string(os.PathListSeparator)))
	out, err := c.CombinedOutput()
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	name := filepath.Base(cmd)
	if text == "" {
		return fmt.Errorf("%s mcp failed: %w", name, err)
	}
	return fmt.Errorf("%s mcp failed: %s", name, text)
}

// readConfigFile reads an agent's config file; one that isn't there yet reads as empty.
func readConfigFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the agent's own config file, in the user's home
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// writeConfigFile writes an agent's config file in place, keeping its mode and a symlink to it (a
// dotfiles repo's); a new one is 0600, since it holds the token.
func writeConfigFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // G703: the agent's config folder
		return err
	}
	return os.WriteFile(path, b, 0o600) //nolint:gosec // G703: the agent's own config file, in the user's home
}

// homeDir is the user's home folder, or under it the folder an environment variable names instead.
func homeDir(env string, under ...string) (string, error) {
	if d := os.Getenv(env); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, under...)...), nil
}
