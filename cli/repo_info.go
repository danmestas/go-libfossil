package cli

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
)

// RepoInfoCmd shows repository statistics.
type RepoInfoCmd struct{}

func (c *RepoInfoCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	db := r.Inner().DB()

	var blobCount, deltaCount, phantomCount int
	var totalSize int64
	var projectCode, serverCode string

	db.QueryRow("SELECT count(*) FROM blob WHERE size >= 0").Scan(&blobCount)
	db.QueryRow("SELECT count(*) FROM delta").Scan(&deltaCount)
	db.QueryRow("SELECT count(*) FROM phantom").Scan(&phantomCount)
	db.QueryRow("SELECT coalesce(sum(size),0) FROM blob WHERE size >= 0").Scan(&totalSize)
	db.QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projectCode)
	db.QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&serverCode)

	fmt.Printf("project-code:  %s\n", projectCode)
	fmt.Printf("server-code:   %s\n", serverCode)
	fmt.Printf("blobs:         %d\n", blobCount)
	fmt.Printf("deltas:        %d\n", deltaCount)
	fmt.Printf("phantoms:      %d\n", phantomCount)
	fmt.Printf("total size:    %d bytes\n", totalSize)
	return printHorizon(db)
}

// printHorizon names the store's horizon, and under a structural one the
// check-ins below it: a boundary hash is the horizon, not a missing parent
// (draft-fossil-horizon-00 §5.4).
func printHorizon(q db.Querier) error {
	sc, err := content.StoreCone(q)
	if err != nil || sc == nil {
		return err
	}
	h, ok := sc.Horizon()
	if !ok {
		return nil
	}
	fmt.Printf("horizon:       %s %s %s\n", h.Hash(), h.Rule(), h.Depth())
	a, err := content.LoadAbsencesContext(context.Background(), q)
	if err != nil {
		return err
	}
	if n := a.BelowHorizon(); n > 0 {
		fmt.Printf("below horizon: %d check-in(s)\n", n)
	}
	return nil
}
