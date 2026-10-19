package cli

import (
	"fmt"
	"os"
	"path/filepath"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
)

// RepoDiffCmd shows changes in the working directory vs a repository version,
// or, given two versions, the changes between them.
type RepoDiffCmd struct {
	Version string `arg:"" optional:"" help:"Version to diff against (default: tip)"`
	To      string `arg:"" optional:"" help:"Second version: diff Version against it instead of the working directory"`
	Path    string `arg:"" optional:"" help:"With two versions, only this file (default: every changed file)"`
	JSON    bool   `help:"With two versions, print a JSON array of {name, unified} instead of text"`
	Dir     string `short:"d" help:"Working directory to compare" default:"."`
	Unified int    `short:"U" help:"Lines of context" default:"3"`
}

// diffEntry is one element of diff's --json array.
type diffEntry struct {
	Name    string `json:"name"`
	Unified string `json:"unified"`
}

func (c *RepoDiffCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	if c.To != "" {
		return c.runBetween(r)
	}
	if c.JSON {
		return fmt.Errorf("--json needs two versions: repo diff --json A B [PATH]")
	}

	rid, err := resolveRID(r, c.Version)
	if err != nil {
		return err
	}

	files, err := r.ListFiles(rid)
	if err != nil {
		return err
	}

	db := r.Inner().DB()
	hasDiff := false

	for _, f := range files {
		diskPath := filepath.Join(c.Dir, f.Name)
		diskData, err := os.ReadFile(diskPath)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("--- a/%s\n+++ /dev/null\n@@ deleted @@\n", f.Name)
				hasDiff = true
				continue
			}
			return err
		}

		fileRid, ok := content.AvailableByUUID(db, f.UUID)
		if !ok {
			return fmt.Errorf("blob %s not found for %s", f.UUID, f.Name)
		}
		repoContent, err := content.Expand(db, fileRid)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}

		repoStr := string(repoContent)
		diskStr := string(diskData)

		if repoStr == diskStr {
			continue
		}

		edits := myers.ComputeEdits(span.URIFromPath(f.Name), repoStr, diskStr)
		diff := fmt.Sprint(gotextdiff.ToUnified("a/"+f.Name, "b/"+f.Name, repoStr, edits))
		if diff != "" {
			fmt.Print(diff)
			hasDiff = true
		}
	}

	if !hasDiff {
		fmt.Println("no changes")
	}
	return nil
}

// runBetween prints the unified diffs between versions c.Version and c.To,
// for c.Path alone or every file that differs. A version that does not exist
// is ExitNotFound; versions that do not differ print nothing (text) or [].
func (c *RepoDiffCmd) runBetween(r *libfossil.Repo) error {
	a, err := resolveVersionExit(r, c.Version)
	if err != nil {
		return err
	}
	b, err := resolveVersionExit(r, c.To)
	if err != nil {
		return err
	}
	diffs, err := r.Diff(a, b, c.Path)
	if err != nil {
		return err
	}
	if c.JSON {
		out := make([]diffEntry, len(diffs))
		for i, d := range diffs {
			out[i] = diffEntry{Name: d.Name, Unified: d.Unified}
		}
		return writeJSON(out)
	}
	for _, d := range diffs {
		fmt.Print(d.Unified)
	}
	return nil
}
