package cli

import (
	"fmt"
	"strings"
)

// RepoStatusCmd shows the checkout's changes against its checked-out version.
type RepoStatusCmd struct {
	Dir string `short:"d" help:"Checkout directory to scan" default:"."`
}

// statusLabels are fossil's changes-command labels for the library's change
// kinds; a sparse checkout reports only within its cone, as Status does.
var statusLabels = map[string]string{
	"modified": "EDITED",
	"added":    "ADDED",
	"deleted":  "DELETED",
	"missing":  "MISSING",
	"renamed":  "RENAMED",
}

func (c *RepoStatusCmd) Run(g *Globals) error {
	_, co, done, err := openWorkingCheckout(g, c.Dir)
	if err != nil {
		return err
	}
	changes, err := co.Status()
	if closeErr := done(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, ch := range changes {
		label, ok := statusLabels[ch.Change]
		if !ok {
			label = strings.ToUpper(ch.Change)
		}
		fmt.Printf("%-8s %s\n", label, ch.Name)
	}
	return nil
}
