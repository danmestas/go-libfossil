package conformance

import "testing"

// artifactGaps marks the artifact cases go-libfossil is known to fail, by
// case ID, grouped by the code at fault, each with the issue tracking its fix.
var artifactGaps = map[string]string{
	// deck.Parse reads a Julian day number as digits with an optional
	// fraction only; Section 4.6.4 form 5 also allows a sign, a leading or
	// trailing point and an exponent, as fossil(1) does.
	"artifact.4.6.4.julian-plus-sign":      "#370",
	"artifact.4.6.4.julian-exponent":       "#370",
	"artifact.4.6.4.julian-exponent-upper": "#370",
	"artifact.4.6.4.julian-leading-point":  "#370",
	"artifact.4.6.4.julian-trailing-point": "#370",
	// manifest.ListFiles refuses a baseline that is itself a delta manifest
	// (#275); Section 4.7.3 of the -01 draft has a reader accept it and
	// resolve the file set one level, as fossil(1) does.
	"artifact.4.7.3.delta-of-delta-one-level": "#371",
}

// TestArtifactGapKeys checks that every marked case exists, and that every
// loss the fuzz test forgives is still a marked gap.
func TestArtifactGapKeys(t *testing.T) {
	ids := map[string]bool{}
	for _, c := range artifactSuiteCases() {
		ids[c.ID] = true
	}
	for id := range artifactGaps {
		if !ids[id] {
			t.Errorf("artifactGaps names %s, which is not a case", id)
		}
	}
	for _, loss := range knownLosses {
		if artifactGaps[loss.caseID] == "" {
			t.Errorf("FuzzArtifactRoundTrip forgives %s, which is no longer a gap; "+
				"remove it from knownLosses", loss.caseID)
		}
	}
}
