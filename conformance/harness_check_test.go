package conformance

import "testing"

// A case whose fixture skips, as one does when fossil(1) is absent, must end
// that case alone. The harness runs each case as a subtest, so the case has to
// skip on the subtest's t: skipping the parent from inside the subtest fails
// the whole run, which is what the Race workflow, with no fossil installed,
// hit. The probe runs check in a subtest of its own and expects it not to
// fail.
func TestCheckSkipEndsOnlyTheCase(t *testing.T) {
	probe := Case{ID: "harness.check.skip-ends-case", Req: "harness"}
	ok := t.Run("probe", func(t *testing.T) {
		check(t, probe, func(t *testing.T) error {
			t.Skip("fixture unavailable")
			return nil
		})
	})
	if !ok {
		t.Fatal("a case that skipped failed its parent")
	}
}
