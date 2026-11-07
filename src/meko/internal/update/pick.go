package update

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// TagPrefix is Meko's release tags' prefix: meko-v1.YYYYMMDD.N.
const TagPrefix = "meko-v"

// Sums is the checksum file every release carries.
const Sums = "SHA256SUMS"

// Version is a release version: three numbers, as 1.20261108.5.
type Version [3]int

// ParseVersion reads a bare version (no prefix, no prerelease part).
func ParseVersion(s string) (Version, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var v Version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return Version{}, false
		}
		v[i] = n
	}
	return v, true
}

// Less orders versions.
func (v Version) Less(o Version) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// Platform is this machine's in release file names: linux or darwin, x64 or arm64.
func Platform() (string, string) {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	return runtime.GOOS, arch
}

// TarballName is a Meko release's tarball for a platform.
func TarballName(version, goos, arch string) string {
	return PackageTarball("meko", version, goos, arch)
}

// PackageTarball is a release's tarball of package name for a platform: Meko's, or an extension's
// that Meko installs from its releases (present-<version>-<os>-<arch>.tar.gz).
func PackageTarball(name, version, goos, arch string) string {
	return fmt.Sprintf("%s-%s-%s-%s.tar.gz", name, version, goos, arch)
}

// Pick is the newest stable release whose tag is one of prefixes plus a version, that carries every
// asset assets(version) names, and whose version isn't in skip. It returns the release and its
// bare version.
func Pick(releases []Release, prefixes []string, assets func(version string) []string,
	skip map[string]bool) (Release, string, bool) {
	var best Release
	var bestV Version
	found := false
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		ver, ok := versionOf(r.Tag, prefixes)
		if !ok || skip[ver] {
			continue
		}
		v, _ := ParseVersion(ver)
		missing := false
		for _, a := range assets(ver) {
			if _, ok := r.Asset(a); !ok {
				missing = true
			}
		}
		if missing {
			continue
		}
		if !found || bestV.Less(v) {
			best, bestV, found = r, v, true
		}
	}
	return best, bestV.String(), found
}

// versionOf is the version a tag names after one of prefixes.
func versionOf(tag string, prefixes []string) (string, bool) {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(tag, p); ok {
			if _, ok := ParseVersion(rest); ok {
				return rest, true
			}
		}
	}
	return "", false
}

// PickMeko is the newest Meko release with a tarball for this platform.
func PickMeko(releases []Release, goos, arch string, bad map[string]bool) (Release, string, bool) {
	return PickPackage(releases, []string{TagPrefix}, []string{"meko"}, goos, arch, bad)
}

// PickPackage is the newest release tagged with one of prefixes that has a tarball for this platform
// of the package by one of names (App.Packages: its name now, then its old ones) and SHA256SUMS,
// skipping the versions in bad.
func PickPackage(releases []Release, prefixes, names []string, goos, arch string,
	bad map[string]bool) (Release, string, bool) {
	var best Release
	var bestV Version
	found := false
	for _, name := range names {
		r, ver, ok := Pick(releases, prefixes, func(v string) []string {
			return []string{PackageTarball(name, v, goos, arch), Sums}
		}, bad)
		if v, _ := ParseVersion(ver); ok && (!found || bestV.Less(v)) {
			best, bestV, found = r, v, true
		}
	}
	return best, bestV.String(), found
}

// PickPackageVersion is the release of the package's version, tagged with one of prefixes, that has
// its tarball for this platform by one of names and SHA256SUMS, whether or not it was rolled back.
func PickPackageVersion(releases []Release, prefixes, names []string, version, goos, arch string) (Release, bool) {
	for _, r := range releases {
		if r.Draft {
			continue
		}
		if v, ok := versionOf(r.Tag, prefixes); !ok || v != version {
			continue
		}
		if _, sums := r.Asset(Sums); !sums {
			continue
		}
		for _, name := range names {
			if _, tar := r.Asset(PackageTarball(name, version, goos, arch)); tar {
				return r, true
			}
		}
	}
	return Release{}, false
}
