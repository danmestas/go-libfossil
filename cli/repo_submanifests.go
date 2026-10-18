package cli

import (
	"errors"
	"fmt"
)

// RepoSubmanifestsCmd groups the sub-manifest settings
// (draft-fossil-submanifests-00): whether new check-ins are written as
// trees, and the partition parameter k they are built with.
type RepoSubmanifestsCmd struct {
	Enable  RepoSubmanifestsEnableCmd  `cmd:"" help:"Write new check-ins as sub-manifest trees"`
	Retune  RepoSubmanifestsRetuneCmd  `cmd:"" help:"Re-pin the partition parameter for later commits"`
	Disable RepoSubmanifestsDisableCmd `cmd:"" help:"Write new check-ins as flat manifests again"`
}

// RepoSubmanifestsEnableCmd turns tree emission on, pinning k the first time.
type RepoSubmanifestsEnableCmd struct {
	K int `name:"k" help:"Partition parameter, 1..12 (default: selected from the newest check-in)"`
}

func (c *RepoSubmanifestsEnableCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	k, warning, err := r.EnableSubmanifests(c.K)
	if err != nil {
		return err
	}
	fmt.Printf("enable-submanifests: on (submanifest-k pinned to %d)\n", k)
	fmt.Println("existing check-ins stay flat; from the next commit, peers below Fossil 2.29 receive\n" +
		"nothing new from this repository.")
	fmt.Printf("\nWARNING: %s\n", warning)
	return nil
}

// RepoSubmanifestsRetuneCmd re-pins k; a re-tune is always explicit.
type RepoSubmanifestsRetuneCmd struct {
	K int `name:"k" help:"New partition parameter, 1..12"`
}

func (c *RepoSubmanifestsRetuneCmd) Run(g *Globals) error {
	if c.K == 0 {
		return errors.New("retune needs an explicit value: --k N")
	}
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	if err := r.RetuneSubmanifests(c.K); err != nil {
		return err
	}
	fmt.Printf("submanifest-k: %d\n", c.K)
	fmt.Println("note: the next commit shares only the sub-manifests whose bytes are unchanged.\n" +
		"No history is rewritten and no reader behaves differently.")
	return nil
}

// RepoSubmanifestsDisableCmd turns tree emission off; existing trees stay.
type RepoSubmanifestsDisableCmd struct{}

func (c *RepoSubmanifestsDisableCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	note, err := r.DisableSubmanifests()
	if err != nil {
		return err
	}
	fmt.Println("enable-submanifests: off")
	if note != "" {
		fmt.Println(note)
	}
	return nil
}
