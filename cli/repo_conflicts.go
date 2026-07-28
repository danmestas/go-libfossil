package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// RepoConflictsCmd groups conflict management operations.
type RepoConflictsCmd struct {
	Ls      RepoConflictsLsCmd      `cmd:"" default:"1" help:"List all conflicts"`
	Show    RepoConflictsShowCmd    `cmd:"" help:"Show all versions of a conflicted file"`
	Pick    RepoConflictsPickCmd    `cmd:"" help:"Resolve by picking one version"`
	Merge   RepoConflictsMergeCmd   `cmd:"" help:"Resolve by re-merging with a different strategy"`
	Extract RepoConflictsExtractCmd `cmd:"" help:"Extract all versions to disk for manual editing"`
	Dir     string                  `short:"d" help:"Checkout directory" default:"."`
}

// RepoConflictsLsCmd lists all conflicts.
type RepoConflictsLsCmd struct{}

func (c *RepoConflictsLsCmd) Run(g *Globals) error {
	return (&RepoConflictsCmd{Dir: "."}).list(g)
}

func (c *RepoConflictsCmd) list(g *Globals) error {
	found := 0

	// Standard merge conflicts (vfile.chnged=5).
	ckout, err := openCheckout(c.Dir)
	if err == nil {
		defer ckout.Close()
		vid, _ := checkoutVid(ckout)
		rows, err := ckout.Query("SELECT pathname FROM vfile WHERE chnged=5 AND vid=?", vid)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var name string
				rows.Scan(&name)
				fmt.Printf("CONFLICT  %s\n", name)
				found++
			}
		}
	}

	// Conflict-fork entries.
	r, err := g.OpenRepo()
	if err == nil {
		defer r.Close()
		inner := r.Inner()
		entries, err := listConflictForkDetails(inner)
		if err == nil {
			for _, e := range entries {
				fmt.Printf("FORK      %s  (base=%d local=%d remote=%d)\n",
					e.filename, e.baseRid, e.localRid, e.remoteRid)
				found++
			}
		}
	}

	if found == 0 {
		fmt.Println("no conflicts")
	}
	return nil
}

type conflictForkEntry struct {
	filename  string
	baseRid   int64
	localRid  int64
	remoteRid int64
	ridKind   int64
}

func listConflictForkDetails(r *repo.Repo) ([]conflictForkEntry, error) {
	var count int
	if r.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='conflict'").Scan(&count); count == 0 {
		return nil, nil
	}
	rows, err := r.DB().Query("SELECT filename, base_rid, local_rid, remote_rid FROM conflict ORDER BY mtime DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []conflictForkEntry
	for rows.Next() {
		var e conflictForkEntry
		rows.Scan(&e.filename, &e.baseRid, &e.localRid, &e.remoteRid)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func loadConflictFork(r *repo.Repo, filename string) (*conflictForkEntry, error) {
	if err := merge.EnsureConflictTable(r); err != nil {
		return nil, err
	}

	var e conflictForkEntry
	e.filename = filename
	err := r.DB().QueryRow(
		"SELECT base_rid, local_rid, remote_rid, rid_kind FROM conflict WHERE filename=?",
		filename,
	).Scan(&e.baseRid, &e.localRid, &e.remoteRid, &e.ridKind)
	if err != nil {
		return nil, fmt.Errorf("%s: load conflict-fork entry: %w", filename, err)
	}
	return &e, nil
}

func expandForkFile(r *repo.Repo, rid int64, filename string, rowKind int64) ([]byte, bool, error) {

	switch rowKind {
	case 0:
		if rid <= 0 {
			return nil, false, nil
		}
		bytes, err := content.Expand(r.DB(), libfossil.FslID(rid))
		if err != nil {
			return nil, true, fmt.Errorf("expand legacy file %q from blob %d: %w", filename, rid, err)
		}
		return bytes, true, nil
	case 1:
		if rid <= 0 {
			return nil, false, nil
		}
		files, err := manifest.ListFiles(r, libfossil.FslID(rid))
		if err != nil {
			return nil, false, fmt.Errorf("list file %q in checkin %d: %w", filename, rid, err)
		}
		for _, file := range files {
			if file.Name != filename {
				continue
			}
			fileRid, ok := content.AvailableByUUID(r.DB(), file.UUID)
			if !ok {
				return nil, true, fmt.Errorf("file %q from checkin %d is unavailable", filename, rid)
			}
			bytes, err := content.Expand(r.DB(), fileRid)
			if err != nil {
				return nil, true, fmt.Errorf("expand file %q from checkin %d: %w", filename, rid, err)
			}
			return bytes, true, nil
		}
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("expand file %q: unknown conflict RID kind %d", filename, rowKind)
	}
}

// RepoConflictsShowCmd shows all versions of a conflicted file.
type RepoConflictsShowCmd struct {
	File string `arg:"" help:"Conflicted file to show"`
}

func (c *RepoConflictsShowCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	inner := r.Inner()
	entry, err := loadConflictFork(inner, c.File)
	if err != nil {
		return err
	}

	base, basePresent, err := expandForkFile(inner, entry.baseRid, c.File, entry.ridKind)
	if err != nil {
		return err
	}
	local, localPresent, err := expandForkFile(inner, entry.localRid, c.File, entry.ridKind)
	if err != nil {
		return err
	}
	remote, remotePresent, err := expandForkFile(inner, entry.remoteRid, c.File, entry.ridKind)
	if err != nil {
		return err
	}

	fmt.Printf("=== BASE (ancestor, rid=%d) ===\n", entry.baseRid)
	if basePresent {
		if _, err := os.Stdout.Write(base); err != nil {
			return err
		}
	} else {
		fmt.Print("<absent>")
	}
	fmt.Printf("\n=== LOCAL (your version, rid=%d) ===\n", entry.localRid)
	if localPresent {
		if _, err := os.Stdout.Write(local); err != nil {
			return err
		}
	} else {
		fmt.Print("<absent>")
	}
	fmt.Printf("\n=== REMOTE (their version, rid=%d) ===\n", entry.remoteRid)
	if remotePresent {
		if _, err := os.Stdout.Write(remote); err != nil {
			return err
		}
	} else {
		fmt.Print("<absent>")
	}
	fmt.Println()
	return nil
}

// RepoConflictsPickCmd resolves a conflict by picking one version.
type RepoConflictsPickCmd struct {
	File   string `arg:"" help:"Conflicted file to resolve"`
	Local  bool   `help:"Keep local version" xor:"version"`
	Remote bool   `help:"Keep remote version" xor:"version"`
	Base   bool   `help:"Revert to base version" xor:"version"`
	Dir    string `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoConflictsPickCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	inner := r.Inner()
	entry, err := loadConflictFork(inner, c.File)
	if err != nil {
		return err
	}

	var picked []byte
	var present bool
	var label string
	switch {
	case c.Remote:
		picked, present, err = expandForkFile(inner, entry.remoteRid, c.File, entry.ridKind)
		label = "remote"
	case c.Base:
		picked, present, err = expandForkFile(inner, entry.baseRid, c.File, entry.ridKind)
		label = "base"
	default:
		picked, present, err = expandForkFile(inner, entry.localRid, c.File, entry.ridKind)
		label = "local"
	}
	if err != nil {
		return err
	}

	outPath := filepath.Join(c.Dir, c.File)
	if present {
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(outPath, picked, 0o644); err != nil {
			return err
		}
	} else if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := merge.ResolveConflictFork(inner, c.File); err != nil {
		return err
	}
	fmt.Printf("resolved: %s (picked %s)\n", c.File, label)
	return nil
}

// RepoConflictsMergeCmd resolves a conflict by re-merging with a specified strategy.
type RepoConflictsMergeCmd struct {
	File     string `arg:"" help:"Conflicted file to re-merge"`
	Strategy string `help:"Strategy to use" default:"three-way"`
	Dir      string `short:"d" help:"Checkout directory" default:"."`
}

func (c *RepoConflictsMergeCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	inner := r.Inner()
	entry, err := loadConflictFork(inner, c.File)
	if err != nil {
		return err
	}

	strat, ok := merge.StrategyByName(c.Strategy)
	if !ok {
		return fmt.Errorf("unknown strategy: %s", c.Strategy)
	}

	base, basePresent, err := expandForkFile(inner, entry.baseRid, c.File, entry.ridKind)
	if err != nil {
		return err
	}
	local, localPresent, err := expandForkFile(inner, entry.localRid, c.File, entry.ridKind)
	if err != nil {
		return err
	}
	remote, remotePresent, err := expandForkFile(inner, entry.remoteRid, c.File, entry.ridKind)
	if err != nil {
		return err
	}
	if !basePresent {
		base = nil
	}
	if !localPresent {
		local = nil
	}
	if !remotePresent {
		remote = nil
	}

	result, err := strat.Merge(base, local, remote)
	if err != nil {
		return err
	}

	outPath := filepath.Join(c.Dir, c.File)
	os.MkdirAll(filepath.Dir(outPath), 0o755)
	if err := os.WriteFile(outPath, result.Content, 0o644); err != nil {
		return err
	}

	if result.Clean {
		if err := merge.ResolveConflictFork(inner, c.File); err != nil {
			return err
		}
		fmt.Printf("resolved: %s (merged with %s, clean)\n", c.File, c.Strategy)
	} else {
		os.WriteFile(outPath+".LOCAL", local, 0o644)
		os.WriteFile(outPath+".BASELINE", base, 0o644)
		os.WriteFile(outPath+".MERGE", remote, 0o644)
		fmt.Printf("merged: %s (%s, %d conflicts remain -- edit and run mark-resolved)\n",
			c.File, c.Strategy, len(result.Conflicts))
	}
	return nil
}

// RepoConflictsExtractCmd extracts all versions to disk for manual editing.
type RepoConflictsExtractCmd struct {
	File string `arg:"" help:"Conflicted file to extract"`
	Dir  string `short:"d" help:"Output directory" default:"."`
}

func (c *RepoConflictsExtractCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	inner := r.Inner()
	entry, err := loadConflictFork(inner, c.File)
	if err != nil {
		return err
	}

	base, basePresent, err := expandForkFile(inner, entry.baseRid, c.File, entry.ridKind)
	if err != nil {
		return err
	}
	local, localPresent, err := expandForkFile(inner, entry.localRid, c.File, entry.ridKind)
	if err != nil {
		return err
	}
	remote, remotePresent, err := expandForkFile(inner, entry.remoteRid, c.File, entry.ridKind)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}

	basePath := filepath.Join(c.Dir, c.File+".BASE")
	localPath := filepath.Join(c.Dir, c.File+".LOCAL")
	remotePath := filepath.Join(c.Dir, c.File+".REMOTE")

	if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
		return err
	}
	if basePresent {
		if err := os.WriteFile(basePath, base, 0o644); err != nil {
			return err
		}
	} else if err := os.Remove(basePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if localPresent {
		if err := os.WriteFile(localPath, local, 0o644); err != nil {
			return err
		}
	} else if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if remotePresent {
		if err := os.WriteFile(remotePath, remote, 0o644); err != nil {
			return err
		}
	} else if err := os.Remove(remotePath); err != nil && !os.IsNotExist(err) {
		return err
	}

	if basePresent {
		fmt.Printf("  %s\n", basePath)
	}
	if localPresent {
		fmt.Printf("  %s\n", localPath)
	}
	if remotePresent {
		fmt.Printf("  %s\n", remotePath)
	}
	return nil
}
