package cli

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/libfossil/internal/content"
)

// RepoStatusCmd shows working directory changes vs the tip version.
type RepoStatusCmd struct {
	Dir string `short:"d" help:"Checkout directory to scan" default:"."`
}

func (c *RepoStatusCmd) Run(g *Globals) error {
	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer r.Close()

	tipRid, err := resolveRID(r, "tip")
	if err != nil {
		fmt.Println("empty repository -- no checkins")
		return nil
	}

	manifestFiles, err := r.ListFiles(tipRid)
	if err != nil {
		return err
	}

	// Build map of manifest files: name -> UUID.
	expected := make(map[string]string)
	for _, f := range manifestFiles {
		expected[f.Name] = f.UUID
	}

	db := r.Inner().DB()

	// Scan working directory.
	seen := make(map[string]bool)
	err = filepath.WalkDir(c.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".fslckout" || name == "_FOSSIL_" || name == ".fsl" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := filepath.Rel(c.Dir, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(filepath.Base(relPath), ".") {
			return nil
		}

		seen[relPath] = true

		uuid, inManifest := expected[relPath]
		if !inManifest {
			fmt.Printf("EXTRA    %s\n", relPath)
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		h := sha1.Sum(data)
		diskHash := hex.EncodeToString(h[:])

		if diskHash != uuid {
			fileRid, ok := content.AvailableByUUID(db, uuid)
			if ok {
				blobData, err := content.Expand(db, fileRid)
				if err == nil {
					bh := sha1.Sum(blobData)
					blobHash := hex.EncodeToString(bh[:])
					if blobHash == diskHash {
						return nil
					}
				}
			}
			fmt.Printf("EDITED   %s\n", relPath)
		}

		return nil
	})
	if err != nil {
		return err
	}

	for name := range expected {
		if !seen[name] {
			fmt.Printf("MISSING  %s\n", name)
		}
	}

	return nil
}
