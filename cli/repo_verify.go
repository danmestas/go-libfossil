package cli

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/verify"
)

// RepoVerifyCmd verifies repository integrity: every held artifact against
// its name, delta chains, phantom records and the derived tables. It reports
// what the store lacks by class (sparse-cone §5.2): transit phantoms, which
// sync still wants, apart from artifacts the store cone deliberately elides,
// which are healthy and never an error.
//
// --seed first verifies a structural store's seed by deepening
// (draft-fossil-horizon-00 §5.9): the history below the horizon is fetched
// from --remote, else the saved last-sync-url, and what it derives is
// compared with what the seed asserted. The store is left a content-horizon
// store, which holds no seed.
type RepoVerifyCmd struct {
	RecoveryFlags
	Seed bool `help:"Verify the structural horizon's seed by fetching the history below it (keeps the content horizon)"`
}

func (c *RepoVerifyCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	mismatches := 0
	if c.Seed {
		required, err := c.requiredContent(r)
		if err != nil {
			return err
		}
		ctx, cancel := cloneContext(context.Background(), c.Timeout)
		defer cancel()
		seeded, err := r.VerifySeed(ctx, required)
		if err != nil {
			return err
		}
		for _, m := range seeded.Mismatches {
			fmt.Printf("SEED %s %s seeded %q, derived %q\n", m.Checkin, m.Tag, m.Seeded, m.Derived)
		}
		mismatches = len(seeded.Mismatches)
		if mismatches == 0 {
			fmt.Printf("seed verified: %d tag(s) match the history below the horizon\n", seeded.Checked)
		}
	}

	report, err := verify.Verify(r.Inner())
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}
	for _, issue := range report.Issues {
		fmt.Printf("ERROR %s %s\n", issue.UUID, issue.Message)
	}
	fmt.Printf("%d artifacts checked, %d transit phantoms, %d elided",
		report.BlobsChecked, report.Phantoms, report.Elided)
	// Horizon §5.4: the manifests below a structural horizon are elided,
	// reported apart from damage, and named as the horizon.
	if report.BelowHorizon > 0 {
		fmt.Printf(", %d check-in(s) below the horizon", report.BelowHorizon)
	}
	fmt.Println()
	if !report.OK() {
		return fmt.Errorf("verification failed: %d issues", len(report.Issues))
	}
	if mismatches > 0 {
		return fmt.Errorf("seed verification failed: %d tag(s) differ from the history below the horizon", mismatches)
	}
	fmt.Println("repository integrity verified")
	return nil
}
