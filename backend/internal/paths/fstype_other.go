//go:build !linux && !darwin

package paths

// fsType isn't known on other platforms, so nothing counts as a network filesystem.
func fsType(string) (string, error) { return "", nil }
