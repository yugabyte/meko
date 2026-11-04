package supervisor

import (
	"fmt"
	"os"
	"strings"
)

// processStart identifies when pid started: the boot's id and the start time in clock ticks since
// boot (field 22 of /proc/<pid>/stat), so neither a reused pid nor a reboot matches it.
func processStart(pid int) (string, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// The command name (field 2) is in parentheses and may hold spaces: count from after it.
	s := string(stat)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(fields) < 20 {
		return "", fmt.Errorf("can't read /proc/%d/stat", pid)
	}
	return strings.TrimSpace(string(boot)) + ":" + fields[19], nil
}
