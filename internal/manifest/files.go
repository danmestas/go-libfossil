package manifest

import (
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

type FileEntry struct {
	Name string
	UUID string
	Perm string
}

func ListFiles(r *repo.Repo, rid libfossil.FslID) ([]FileEntry, error) {
	return ListFilesCached(r, rid, nil)
}

// ListFilesCached is ListFiles that serves the checkin manifest -- and, for a
// delta manifest, its baseline -- through cache. A nil cache is exactly
// ListFiles (content.Cache's nil receiver falls through to content.Expand).
// A live cache helps only a caller listing many overlapping revisions, such as
// annotate walking one file's history; see GetManifestCached.
func ListFilesCached(r *repo.Repo, rid libfossil.FslID, cache *content.Cache) ([]FileEntry, error) {
	d, err := GetManifestCached(r, rid, cache)
	if err != nil {
		return nil, fmt.Errorf("manifest.ListFiles: %w", err)
	}
	if d.B == "" {
		return fCardsToEntries(d.F), nil
	}
	baseRid, ok := content.AvailableByUUID(r.DB(), d.B)
	if !ok {
		return nil, fmt.Errorf("manifest.ListFiles: baseline %s not found", d.B)
	}
	baseData, err := cache.Expand(r.DB(), baseRid)
	if err != nil {
		return nil, fmt.Errorf("manifest.ListFiles: expand baseline: %w", err)
	}
	baseDeck, err := deck.Parse(baseData)
	if err != nil {
		return nil, fmt.Errorf("manifest.ListFiles: parse baseline: %w", err)
	}
	fileMap := make(map[string]FileEntry)
	for _, f := range baseDeck.F {
		fileMap[f.Name] = FileEntry{Name: f.Name, UUID: f.UUID, Perm: f.Perm}
	}
	for _, f := range d.F {
		if f.UUID == "" {
			delete(fileMap, f.Name)
		} else {
			fileMap[f.Name] = FileEntry{Name: f.Name, UUID: f.UUID, Perm: f.Perm}
		}
	}
	entries := make([]FileEntry, 0, len(fileMap))
	for _, e := range fileMap {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func fCardsToEntries(fCards []deck.FileCard) []FileEntry {
	entries := make([]FileEntry, len(fCards))
	for i, f := range fCards {
		entries[i] = FileEntry{Name: f.Name, UUID: f.UUID, Perm: f.Perm}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// MergeParentFiles returns supplied with every file tracked at parentRID
// that is not already in supplied appended. Names in supplied win over the
// parent's entry; the parent's untouched files have their content expanded
// from the repo so the resulting slice is a full-tree set ready for Checkin.
//
// parentRID == 0 returns supplied unchanged.
func MergeParentFiles(r *repo.Repo, parentRID libfossil.FslID, supplied []File) ([]File, error) {
	if parentRID == 0 {
		return supplied, nil
	}
	parentFiles, err := ListFiles(r, parentRID)
	if err != nil {
		return nil, fmt.Errorf("manifest.MergeParentFiles: %w", err)
	}
	suppliedNames := make(map[string]struct{}, len(supplied))
	for _, f := range supplied {
		suppliedNames[f.Name] = struct{}{}
	}
	merged := supplied
	for _, pf := range parentFiles {
		if _, ok := suppliedNames[pf.Name]; ok {
			continue
		}
		baseRid, ok := content.AvailableByUUID(r.DB(), pf.UUID)
		if !ok {
			return nil, fmt.Errorf("manifest.MergeParentFiles: blob %s for %s not found", pf.UUID, pf.Name)
		}
		// Bare Expand: each untouched parent file is expanded once to build a
		// single commit's full-tree file set. Distinct blobs, no overlap.
		data, err := content.Expand(r.DB(), baseRid)
		if err != nil {
			return nil, fmt.Errorf("manifest.MergeParentFiles: expand %s: %w", pf.Name, err)
		}
		merged = append(merged, File{
			Name:    pf.Name,
			Content: data,
			Perm:    pf.Perm,
		})
	}
	return merged, nil
}
