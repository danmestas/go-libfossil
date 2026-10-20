package conformance

import (
	"errors"
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// Cryptographic content addressing prevents authoring a real cycle. The probe
// primes only walker identity state; its node still goes through real storage,
// hash verification, parsing and treeWalk.step's repeated-artifact guard.
func smGuardCases() []smCase {
	return []smCase{
		smRepoCase("submanifest.5.1.cycle-guard-boundary", "SM-5.1-5", smCycleGuard),
		smRepoCase("submanifest.10.cycle-guard-boundary", "SM-10-1", smCycleGuard),
		smRepoCase("submanifest.10.visit-guard-boundary", "SM-10-1", smVisitGuard),
	}
}

func smGuardNode(f *smFixture) (string, error) {
	refs, err := f.leaves([]string{"guard/a"})
	if err != nil {
		return "", err
	}
	uuid := refs[0].UUID
	rid, held := content.AvailableByUUID(f.r.DB(), uuid)
	if !held {
		return "", fmt.Errorf("guard fixture node %s is not held", uuid)
	}
	data, err := content.Expand(f.r.DB(), rid)
	if err != nil {
		return "", err
	}
	if hash.SHA3(data) != uuid {
		return "", fmt.Errorf("guard fixture node %s failed hash verification", uuid)
	}
	return uuid, nil
}

func smCycleGuard(f *smFixture) error {
	uuid, err := smGuardNode(f)
	if err != nil {
		return err
	}
	return smGuardFault(manifest.ProbeTreeCycle(f.r.DB(), uuid), uuid,
		"the reference closes a cycle")
}

func smVisitGuard(f *smFixture) error {
	uuid, err := smGuardNode(f)
	if err != nil {
		return err
	}
	lastAllowed, firstRefused := manifest.ProbeTreeVisitLimit(f.r.DB(), uuid)
	if lastAllowed != nil {
		return fmt.Errorf("last allowed resolution refused: %w", lastAllowed)
	}
	return smGuardFault(firstRefused, uuid, "the tree resolves more than 1048576 sub-manifests")
}

func smGuardFault(err error, uuid, reason string) error {
	var fault *manifest.SubmanifestError
	if !errors.As(err, &fault) {
		return fmt.Errorf("guard error %v, want SubmanifestError", err)
	}
	if fault.UUID != uuid {
		return fmt.Errorf("guard fault UUID %q, want %q", fault.UUID, uuid)
	}
	if fault.Missing {
		return fmt.Errorf("guard classified a held artifact as missing")
	}
	return want("guard fault reason", fault.Reason, reason)
}

func TestSubmanifestGuards(t *testing.T) {
	for _, c := range smGuardCases() {
		check(t, c.Case, func() error { return c.run(t) })
	}
}
