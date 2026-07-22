package search

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// maxBinaryProbe is the number of bytes checked for null bytes to detect binary files.
const maxBinaryProbe = 8192

// isBinary returns true if data contains a null byte in the first maxBinaryProbe bytes.
func isBinary(data []byte) bool {
	if data == nil {
		panic("isBinary: nil data")
	}
	probe := data
	if len(probe) > maxBinaryProbe {
		probe = probe[:maxBinaryProbe]
	}
	return bytes.ContainsRune(probe, 0)
}

// RebuildIndex walks the trunk tip manifest, expands blob content,
// skips binaries and phantoms, and populates fts_content.
// No-ops if already current.
//
// Panics if idx is nil (TigerStyle precondition).
func (idx *Index) RebuildIndex() error {
	if idx == nil {
		panic("search.RebuildIndex: nil *Index")
	}

	d := idx.repo.DB()

	tip, err := trunkTip(d)
	if err != nil {
		return fmt.Errorf("search.RebuildIndex: %w", err)
	}
	if tip == 0 {
		return nil // empty repo, nothing to index
	}

	current, err := indexedRID(d)
	if err != nil {
		return fmt.Errorf("search.RebuildIndex: %w", err)
	}
	if tip == current {
		return nil // already up to date
	}

	if err := rebuildPopulate(idx.repo, d, tip); err != nil {
		return err
	}

	// Postcondition: verify the meta update took hold.
	stored, err := indexedRID(d)
	if err != nil {
		return fmt.Errorf("search.RebuildIndex: verify meta: %w", err)
	}
	if stored != tip {
		panic("search.RebuildIndex: postcondition: indexed_rid != tip after update")
	}

	return nil
}

// rebuildPopulate clears fts_content, walks the manifest at tip,
// expands each text blob, and inserts into the FTS index.
func rebuildPopulate(r *repo.Repo, d db.Querier, tip libfossil.FslID) error {
	if _, err := d.Exec("DELETE FROM fts_content"); err != nil {
		return fmt.Errorf("search.RebuildIndex: clear: %w", err)
	}

	files, err := manifest.ListFiles(r, tip)
	if err != nil {
		return fmt.Errorf("search.RebuildIndex: list files: %w", err)
	}

	for _, f := range files {
		rid, ok := content.AvailableByUUID(d, f.UUID)
		if !ok {
			continue // phantom — blob not yet received
		}

		data, err := content.Expand(d, rid)
		if err != nil {
			continue // phantom or corrupt — skip
		}

		if isBinary(data) {
			continue
		}

		if _, err := d.Exec(
			"INSERT INTO fts_content(path, content) VALUES(?, ?)",
			f.Name, string(data),
		); err != nil {
			return fmt.Errorf("search.RebuildIndex: insert %s: %w", f.Name, err)
		}
	}

	if _, err := d.Exec(
		"INSERT OR REPLACE INTO fts_meta(key, value) VALUES('indexed_rid', ?)",
		strconv.FormatInt(int64(tip), 10),
	); err != nil {
		return fmt.Errorf("search.RebuildIndex: update meta: %w", err)
	}

	return nil
}
