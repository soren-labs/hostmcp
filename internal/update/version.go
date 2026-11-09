package update

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// Version is a release version: MAJOR.MINOR.PATCH, or MAJOR.MINOR.PATCH-rc.N
// for a release candidate that only canary nodes install.
type Version struct {
	Major, Minor, Patch uint64
	RC                  uint64 // 0 for a stable release
}

var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-rc\.([1-9][0-9]*))?$`)

// ParseVersion accepts only release versions, without a "v" prefix. Development
// builds ("0.3.0-dev") and other prerelease forms are rejected, so they are
// never replaced automatically.
func ParseVersion(s string) (Version, error) {
	m := releaseVersion.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("%q is not a release version (X.Y.Z or X.Y.Z-rc.N)", s)
	}
	var v Version
	for i, dst := range []*uint64{&v.Major, &v.Minor, &v.Patch, &v.RC} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return Version{}, errors.New("version component out of range")
		}
		*dst = n
	}
	return v, nil
}

func (v Version) Stable() bool { return v.RC == 0 }

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.RC > 0 {
		s += fmt.Sprintf("-rc.%d", v.RC)
	}
	return s
}

// Less orders versions; a release candidate sorts before its stable release.
func (v Version) Less(o Version) bool {
	for _, p := range [][2]uint64{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if p[0] != p[1] {
			return p[0] < p[1]
		}
	}
	switch {
	case v.RC == o.RC:
		return false
	case v.RC == 0:
		return false
	case o.RC == 0:
		return true
	}
	return v.RC < o.RC
}
