package cli

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/manifest"
)

// RepoSubmanifestsCmd groups the sub-manifest settings
// (draft-fossil-submanifests-00): whether new check-ins are written as
// trees, and the partition parameter k they are built with.
type RepoSubmanifestsCmd struct {
	Enable  RepoSubmanifestsEnableCmd  `cmd:"" help:"Write new check-ins as sub-manifest trees"`
	Retune  RepoSubmanifestsRetuneCmd  `cmd:"" help:"Re-pin the partition parameter for later commits"`
	Disable RepoSubmanifestsDisableCmd `cmd:"" help:"Write new check-ins as flat manifests again"`
	Status  RepoSubmanifestsStatusCmd  `cmd:"" help:"Show the sub-manifest settings and whether trees exist"`
}

// RepoSubmanifestsEnableCmd turns tree emission on, pinning k the first time.
type RepoSubmanifestsEnableCmd struct {
	K int `name:"k" help:"Partition parameter, 1..12 (default: selected from the main-branch tip)"`
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
	K int `name:"k" required:"" help:"New partition parameter, 1..12"`
}

func (c *RepoSubmanifestsRetuneCmd) Run(g *Globals) error {
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

	if err := r.DisableSubmanifests(); err != nil {
		return err
	}
	fmt.Println("enable-submanifests: off")
	seen, err := manifest.SeenSubmanifest(r.Inner().DB())
	if err != nil {
		return err
	}
	if seen {
		fmt.Println(manifest.SubmanifestDisableNote)
	}
	return nil
}

// RepoSubmanifestsStatusCmd prints the three settings and whether the
// repository holds any S-bearing artifact.
type RepoSubmanifestsStatusCmd struct{}

func (c *RepoSubmanifestsStatusCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	setting := func(name, unset string) string {
		v, err := r.Config(name)
		if err != nil || v == "" {
			return unset
		}
		return v
	}
	seen, err := manifest.SeenSubmanifest(r.Inner().DB())
	if err != nil {
		return err
	}
	fmt.Printf("%-24s %s\n", manifest.SettingEnableSubmanifests+":", setting(manifest.SettingEnableSubmanifests, "off"))
	fmt.Printf("%-24s %s\n", manifest.SettingSubmanifestK+":", setting(manifest.SettingSubmanifestK, "(unpinned)"))
	fmt.Printf("%-24s %s\n", manifest.SettingAutoRetune+":", setting(manifest.SettingAutoRetune, "off"))
	fmt.Printf("%-24s %s\n", "trees in repository:", map[bool]string{true: "yes", false: "no"}[seen])
	return nil
}
