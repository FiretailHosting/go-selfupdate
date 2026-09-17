package selfupdate

import (
	"strconv"
	"strings"
)

// Newer reports whether release tag "candidate" is later than "current".
//
// A build with no version ("" or "dev") counts as older than everything, so a
// developer build offers the newest release instead of claiming to be current.
func Newer(candidate, current string) bool {
	if candidate == "" {
		return false
	}

	if current == "" || current == "dev" {
		return true
	}

	return Compare(candidate, current) > 0
}

// Compare orders two semver-ish tags, returning -1, 0 or 1.
//
// Tags may carry a leading "v" and a prerelease or build suffix. The suffix is
// only a tiebreaker: 1.2.0 outranks 1.2.0-rc1.
func Compare(a, b string) int {
	aNums, aPre := parseVersion(a)
	bNums, bPre := parseVersion(b)

	for i := 0; i < 3; i++ {
		if aNums[i] != bNums[i] {
			if aNums[i] > bNums[i] {
				return 1
			}
			return -1
		}
	}

	switch {
	case aPre == bPre:
		return 0
	case aPre == "": // a release outranks its own prereleases
		return 1
	case bPre == "":
		return -1
	case aPre > bPre:
		return 1
	default:
		return -1
	}
}

func parseVersion(tag string) (nums [3]int, prerelease string) {
	tag = strings.TrimSpace(tag)
	tag = strings.TrimPrefix(tag, "v")

	if i := strings.IndexAny(tag, "-+"); i >= 0 {
		prerelease = tag[i+1:]
		tag = tag[:i]
	}

	for i, part := range strings.SplitN(tag, ".", 3) {
		if i > 2 {
			break
		}
		nums[i], _ = strconv.Atoi(part)
	}

	return nums, prerelease
}
