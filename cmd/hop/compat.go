package main

import (
	"fmt"
	"strconv"
	"strings"

	"hop.vokh.dev/internal/proto"
)

// preReporting stands in for the release of a server that doesn't report one.
// Reporting arrived in v1.1.0, so a silent server is some v1.0.x, and for
// comparing major and minor that is all that matters.
const preReporting = "v1.0.0"

// serverBehind returns a one-line note when the server's release is older than
// this hop's, or "" when it isn't or the comparison can't be made.
//
// Only major and minor count. Patch releases fix things without adding any, so
// a server a patch behind has nothing this hop would miss, and warning about it
// would teach people to ignore the line. Protocol mismatches never reach here:
// they are refused outright.
func serverBehind(info proto.ServerInfo, ours string) string {
	theirs := info.Release
	if theirs == "" {
		theirs = preReporting
	}
	if !olderRelease(theirs, ours) {
		return ""
	}
	name := "hopd " + info.Release
	if info.Release == "" {
		name = "a hopd from before v1.1.0"
	}
	return fmt.Sprintf("the server runs %s, older than this hop (%s); features added since may not work until it is upgraded", name, ours)
}

// olderRelease reports whether release a is older than b by major or minor.
// Anything that doesn't parse as vMAJOR.MINOR[.PATCH] compares as not older, so
// a development build never produces a warning it can't justify.
func olderRelease(a, b string) bool {
	amaj, amin, ok := majorMinor(a)
	if !ok {
		return false
	}
	bmaj, bmin, ok := majorMinor(b)
	if !ok {
		return false
	}
	if amaj != bmaj {
		return amaj < bmaj
	}
	return amin < bmin
}

func majorMinor(v string) (major, minor int, ok bool) {
	v, found := strings.CutPrefix(v, "v")
	if !found {
		return 0, 0, false
	}
	// Pre-release and build suffixes ("-rc.1", "+dirty") say nothing about
	// which features are in.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return major, minor, true
}
