package paths

import "golang.org/x/sys/unix"

// fsType returns the statfs filesystem name (apfs, nfs, smbfs, macfuse, ...).
func fsType(path string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", err
	}
	return unix.ByteSliceToString(st.Fstypename[:]), nil
}
