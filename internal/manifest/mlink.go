package manifest

import "strings"

// maxMlinkMergeParents bounds the merge-parent lookup loop in canonical parent
// resolution. TigerStyle: every loop needs an explicit bound. A check-in with
// more merge parents than this is almost certainly corrupt input, not a
// legitimate octopus merge.
const maxMlinkMergeParents = 1024

// permToMperm converts a Fossil F-card permission string to the mlink.mperm
// encoding used by canonical Fossil (src/manifest.c add_one_mlink): 0 =
// regular file, 1 = executable, 2 = symlink.
//
// Canonical manifest_file_mperm (src/manifest.c:1482-1492) does a substring
// test (strstr), not an exact match: perm fields can carry more than one
// character (e.g. Fossil's " w" rename placeholder — see #51), and
// internal/deck/parse.go:194 assigns the F-card perm field verbatim from
// remote input over xfer. An exact match would silently drop the
// executable bit for any multi-character perm string containing "x", which
// is the exact invariant PR #48 landed to protect. x is tested before l to
// match canonical's check order.
func permToMperm(perm string) int64 {
	switch {
	case strings.Contains(perm, "x"):
		return 1
	case strings.Contains(perm, "l"):
		return 2
	default:
		return 0
	}
}
