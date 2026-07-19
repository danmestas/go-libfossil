package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/libfossil/internal/content"
)

// RepoExtractCmd extracts files from a version.
type RepoExtractCmd struct {
	Version string   `help:"Version to extract from (default: tip)"`
	Files   []string `arg:"" optional:"" help:"Files to extract (default: all)"`
	Dir     string   `short:"d" help:"Output directory" default:"."`
}

func (c *RepoExtractCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	rid, err := resolveRID(r, c.Version)
	if err != nil {
		return err
	}

	allFiles, err := r.ListFiles(rid)
	if err != nil {
		return err
	}

	db := r.Inner().DB()

	// Filter to requested files if specified.
	type fileInfo struct {
		Name string
		UUID string
		Perm string
	}
	var files []fileInfo
	if len(c.Files) > 0 {
		wanted := make(map[string]bool)
		for _, f := range c.Files {
			wanted[f] = true
		}
		for _, f := range allFiles {
			if wanted[f.Name] {
				files = append(files, fileInfo{f.Name, f.UUID, f.Perm})
			}
		}
		if len(files) == 0 {
			return fmt.Errorf("no matching files found")
		}
	} else {
		for _, f := range allFiles {
			files = append(files, fileInfo{f.Name, f.UUID, f.Perm})
		}
	}

	for _, f := range files {
		fileRid, ok := content.AvailableByUUID(db, f.UUID)
		if !ok {
			return fmt.Errorf("blob %s not found for %s", f.UUID, f.Name)
		}
		data, err := content.Expand(db, fileRid)
		if err != nil {
			return fmt.Errorf("expanding %s: %w", f.Name, err)
		}

		outPath := filepath.Join(c.Dir, f.Name)
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}

		perm := os.FileMode(0o644)
		if f.Perm == "x" {
			perm = 0o755
		}
		if err := os.WriteFile(outPath, data, perm); err != nil {
			return err
		}
		fmt.Println(f.Name)
	}
	return nil
}
