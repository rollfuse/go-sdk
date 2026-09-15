package rollfuse

import (
	"strconv"
	"strings"
)

// semanticVersion is the major.minor.patch triple a semver clause
// compares by: ordering follows semantic version rules ("1.9.0" <
// "1.10.0"), never lexical string ordering. Only the release triple is
// compared — pre-release/build metadata are outside this evaluator's
// closed operator set, matching every fixture vector.
type semanticVersion struct {
	major, minor, patch int
}

// parseSemver extracts the major.minor.patch triple from s, tolerating a
// leading "v" and any trailing pre-release/build suffix. ok is false for
// anything that does not begin with three dot-separated non-negative
// integers.
func parseSemver(s string) (semanticVersion, bool) {
	s = strings.TrimPrefix(s, "v")

	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}

	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return semanticVersion{}, false
	}

	nums := make([]int, 3)

	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return semanticVersion{}, false
		}

		nums[i] = n
	}

	return semanticVersion{major: nums[0], minor: nums[1], patch: nums[2]}, true
}

func (a semanticVersion) compare(b semanticVersion) int {
	switch {
	case a.major != b.major:
		return sign(a.major - b.major)
	case a.minor != b.minor:
		return sign(a.minor - b.minor)
	default:
		return sign(a.patch - b.patch)
	}
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	default:
		return 0
	}
}

// matchSemver implements the four semver comparison operators. Either
// side failing to parse as a semantic version evaluates the clause false
// rather than erroring.
func matchSemver(op ClauseOp, subject, literal string) (bool, string) {
	subjectVersion, ok := parseSemver(subject)
	if !ok {
		return false, "attribute value \"" + subject + "\" is not a valid semantic version"
	}

	literalVersion, ok := parseSemver(literal)
	if !ok {
		return false, "clause literal \"" + literal + "\" is not a valid semantic version"
	}

	cmp := subjectVersion.compare(literalVersion)

	switch op {
	case OpSemverGT:
		return cmp > 0, ""
	case OpSemverGTE:
		return cmp >= 0, ""
	case OpSemverLT:
		return cmp < 0, ""
	case OpSemverLTE:
		return cmp <= 0, ""
	default:
		return false, ""
	}
}
