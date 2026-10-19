package cli

import (
	"database/sql"
	"errors"
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
	K *int `name:"k" help:"Partition parameter, 1..12 (default: selected from the main-branch tip)"`
}

func (c *RepoSubmanifestsEnableCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	// An absent --k selects; a given one must be 1..12 (§8.1), so --k 0 is
	// not the library's "select".
	k := 0
	if c.K != nil {
		if *c.K < manifest.MinPartitionK || *c.K > manifest.MaxPartitionK {
			return fmt.Errorf("--k must be %d..%d (got %d)", manifest.MinPartitionK, manifest.MaxPartitionK, *c.K)
		}
		k = *c.K
	}
	k, warning, err := r.EnableSubmanifests(k)
	if err != nil {
		return err
	}
	fmt.Printf("enable-submanifests: on (submanifest-k pinned to %d)\n", k)
	fmt.Println("existing check-ins stay flat; from the next commit, peers that do not announce\n" +
		"sub-manifest support (every fossil) receive nothing new from this repository.")
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

	q := r.Inner().DB()
	values := map[string]string{}
	for _, name := range []string{manifest.SettingEnableSubmanifests, manifest.SettingSubmanifestK,
		manifest.SettingAutoRetune} {
		var v sql.NullString
		err := q.QueryRow("SELECT value FROM config WHERE name=?", name).Scan(&v)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		values[name] = v.String
	}
	seen, err := manifest.SeenSubmanifest(q)
	if err != nil {
		return err
	}
	or := func(v, unset string) string {
		if v == "" {
			return unset
		}
		return v
	}
	k := or(values[manifest.SettingSubmanifestK], "(unpinned)")
	if raw := values[manifest.SettingSubmanifestK]; raw != "" {
		if _, ok := manifest.ParseSettingK(raw); !ok {
			k = fmt.Sprintf("%s (invalid: outside %d..%d; commits fall back to flat manifests)",
				raw, manifest.MinPartitionK, manifest.MaxPartitionK)
		}
	}
	fmt.Printf("%-24s %s\n", manifest.SettingEnableSubmanifests+":", or(values[manifest.SettingEnableSubmanifests], "off"))
	fmt.Printf("%-24s %s\n", manifest.SettingSubmanifestK+":", k)
	fmt.Printf("%-24s %s\n", manifest.SettingAutoRetune+":", or(values[manifest.SettingAutoRetune], "off"))
	fmt.Printf("%-24s %s\n", "trees in repository:", map[bool]string{true: "yes", false: "no"}[seen])
	return nil
}
