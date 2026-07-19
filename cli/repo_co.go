package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/libfossil/internal/content"
)

// RepoCoCmd checks out a version to the working directory.
type RepoCoCmd struct {
	Version string `arg:"" optional:"" help:"Version to checkout (default: tip)"`
	Dir     string `short:"d" help:"Output directory (default: current dir)" default:"."`
	Force   bool   `help:"Overwrite existing files"`
}

func (c *RepoCoCmd) Run(g *Globals) error {
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
	for _, f := range files {
		fileRid, ok := content.AvailableByUUID(db, f.UUID)
		if !ok {
			return fmt.Errorf("blob %s not found for file %s", f.UUID, f.Name)
		}
		data, err := content.Expand(db, fileRid)
		if err != nil {
			return fmt.Errorf("expanding %s: %w", f.Name, err)
		}

		outPath := filepath.Join(c.Dir, f.Name)
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}

		if !c.Force {
			if _, err := os.Stat(outPath); err == nil {
				return fmt.Errorf("file exists: %s (use --force to overwrite)", outPath)
			}
		}

		perm := os.FileMode(0o644)
		if f.Perm == "x" {
			perm = 0o755
		}
		if err := os.WriteFile(outPath, data, perm); err != nil {
			return err
		}

		fmt.Printf("  %s\n", f.Name)
	}

	fmt.Printf("checked out %d files\n", len(files))
	return nil
}
