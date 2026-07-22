package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
)

// RepoDiffCmd shows changes in the working directory vs a repository version.
type RepoDiffCmd struct {
	Version string `arg:"" optional:"" help:"Version to diff against (default: tip)"`
	Dir     string `short:"d" help:"Working directory to compare" default:"."`
	Unified int    `short:"U" help:"Lines of context" default:"3"`
}

func (c *RepoDiffCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

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
