package paths

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// fsType reads the mount type of the mount holding path from /proc/self/mountinfo, which
// names FUSE filesystems fully (fuse.sshfs), unlike statfs.
func fsType(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	best, bestType := "", ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		mount, typ, ok := parseMountinfo(sc.Text())
		if ok && within(real, mount) && len(mount) >= len(best) {
			best, bestType = mount, typ
		}
	}
	return bestType, sc.Err()
}

// parseMountinfo returns a mountinfo line's mount point and filesystem type: the fifth field,
// and the first field after the " - " separator.
func parseMountinfo(line string) (mount, typ string, ok bool) {
	left, right, found := strings.Cut(line, " - ")
	if !found {
		return "", "", false
	}
	l, r := strings.Fields(left), strings.Fields(right)
	if len(l) < 5 || len(r) < 1 {
		return "", "", false
	}
	return unescapeOctal(l[4]), r[0], true
}

// unescapeOctal undoes mountinfo's \040-style escapes for spaces, tabs and backslashes.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func within(path, mount string) bool {
	return mount == "/" || path == mount || strings.HasPrefix(path, mount+"/")
}
