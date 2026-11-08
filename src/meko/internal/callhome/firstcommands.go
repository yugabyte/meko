package callhome

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"time"
)

// FirstCommandsMax is how many commands after a first install are recorded and reported.
const FirstCommandsMax = 15

// UnknownCommand is the name a command Meko doesn't know is recorded under: what was typed could
// be anything, even a pasted token.
const UnknownCommand = "unknown"

// The first commands' file (paths.Layout.FirstCommands) is JSON lines: a header that a first
// meko install writes, then one Command a line, which every meko and meko-cli run appends while
// there are fewer than FirstCommandsMax. Without the file nothing is recorded: an install from
// before it, or one that wasn't a first install.
type firstCommandsHeader struct {
	Started int64  `json:"started"` // when the install began tracking, in ms since 1970
	Version string `json:"version"`
}

// flagName is what a flag's name looks like (they're fixed in the CLI's code): anything else read
// from the file is dropped, so the report never carries text that isn't a flag's name.
var flagName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// StartFirstCommands begins recording a first install's commands at path, if it isn't already:
// a data folder kept from an earlier install keeps its list.
func StartFirstCommands(path, version string, now time.Time) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	line, err := json.Marshal(firstCommandsHeader{Started: now.UnixMilli(), Version: version})
	if err != nil {
		_ = f.Close()
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return errors.Join(err, f.Close())
}

// FirstCommands is what the file holds: when tracking began and the commands so far, at most
// FirstCommandsMax.
type FirstCommands struct {
	Started  time.Time
	Commands []Command
}

// ReadFirstCommands reads the file at path. known says which command names may be reported; any
// other is UnknownCommand, and a flag that isn't shaped like a flag's name is dropped. A missing
// file is fs.ErrNotExist; a line that can't be read is skipped.
func ReadFirstCommands(path string, known func(string) bool) (FirstCommands, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the data folder's own file
	if err != nil {
		return FirstCommands{}, err
	}
	var out FirstCommands
	sc := bufio.NewScanner(bytes.NewReader(b))
	for first := true; sc.Scan(); first = false {
		if first {
			var h firstCommandsHeader
			if json.Unmarshal(sc.Bytes(), &h) == nil && h.Started > 0 {
				out.Started = time.UnixMilli(h.Started)
			}
			continue
		}
		var c Command
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		if len(out.Commands) == FirstCommandsMax {
			break // two commands at once can append a 16th
		}
		if known == nil || !known(c.Cmd) {
			c.Cmd = UnknownCommand
		}
		flags := []string{}
		for _, f := range c.Flags {
			if flagName.MatchString(f) {
				flags = append(flags, f)
			}
		}
		c.Flags = flags
		c.N = len(out.Commands) + 1
		out.Commands = append(out.Commands, c)
	}
	return out, sc.Err()
}

// Full reports whether no more commands are recorded.
func (f FirstCommands) Full() bool { return len(f.Commands) >= FirstCommandsMax }

// Next is c as the list's next command, numbered and timed from the start of tracking; false when
// the list is full.
func (f FirstCommands) Next(c Command, now time.Time) (Command, bool) {
	if f.Full() {
		return Command{}, false
	}
	c.N = len(f.Commands) + 1
	if !f.Started.IsZero() && now.After(f.Started) {
		c.MinsSinceInstall = int64(now.Sub(f.Started) / time.Minute)
	}
	if c.Flags == nil {
		c.Flags = []string{}
	} else {
		c.Flags = slices.Sorted(slices.Values(c.Flags))
	}
	return c, true
}

// RecordCommand appends c to the file at path while it has fewer than FirstCommandsMax. It
// creates nothing: without the file, nothing is recorded.
func RecordCommand(path string, c Command, now time.Time) error {
	list, err := ReadFirstCommands(path, func(string) bool { return true })
	if err != nil {
		return err
	}
	c, ok := list.Next(c, now)
	if !ok {
		return nil
	}
	line, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// One short line a write: appends from two commands at once don't interleave.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // G304: the data folder's own file
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return errors.Join(err, f.Close())
}
