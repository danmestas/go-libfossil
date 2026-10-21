package cli

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/verify"
)

// RepoVerifyCmd verifies repository integrity: every held artifact against
// its name, delta chains, phantom records and the derived tables. It reports
// what the store lacks by class (sparse-cone §5.2): transit phantoms, which
// sync still wants, apart from artifacts the store cone deliberately elides,
// which are healthy and never an error.
type RepoVerifyCmd struct{}

func (c *RepoVerifyCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	report, err := verify.Verify(r.Inner())
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}
	for _, issue := range report.Issues {
		fmt.Printf("ERROR %s %s\n", issue.UUID, issue.Message)
	}
	fmt.Printf("%d artifacts checked, %d transit phantoms, %d elided\n",
		report.BlobsChecked, report.Phantoms, report.Elided)
	if !report.OK() {
		return fmt.Errorf("verification failed: %d issues", len(report.Issues))
	}
	fmt.Println("repository integrity verified")
	return nil
}
