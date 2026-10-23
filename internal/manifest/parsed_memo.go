package manifest

import (
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// A sweep reads each check-in as a child, again as its child's parent, and
// again as a delta baseline, and parsing a large manifest dominates each read.
// The parsed deck and file views of the few most recent check-ins are kept in
// the sweep's content.Cache beside their bytes, so each is built once. Both are
// shared: no caller may modify them.
type (
	parsedKey   libfossil.FslID // -> *deck.Deck
	filesKey    libfossil.FslID // -> *manifestFiles
	baselineKey string          // baseline UUID -> libfossil.FslID
)

// errExpandManifest marks a manifest whose bytes could not be expanded, as
// against bytes that expanded but do not parse.
var errExpandManifest = errors.New("expand manifest")

// rememberCheckin keeps d, the parse of the check-in manifest at rid.
func rememberCheckin(cache *content.Cache, rid libfossil.FslID, d *deck.Deck) {
	if d.Type == deck.Checkin {
		cache.SetDerived(parsedKey(rid), d)
	}
}

// parseManifestAt expands and parses the manifest at rid. A check-in is served
// from, and kept in, cache's derived memo.
func parseManifestAt(q db.Querier, cache *content.Cache, rid libfossil.FslID) (*deck.Deck, error) {
	if v, ok := cache.Derived(parsedKey(rid)); ok {
		return v.(*deck.Deck), nil
	}
	data, err := expandManifestBytes(q, cache, rid)
	if err != nil {
		return nil, fmt.Errorf("%w rid=%d: %w", errExpandManifest, rid, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("manifest rid=%d is empty", rid)
	}
	d, err := deck.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse manifest rid=%d: %w", rid, err)
	}
	rememberCheckin(cache, rid, d)
	return d, nil
}

// rememberFiles keeps mf, the file views of the check-in manifest at rid.
func rememberFiles(cache *content.Cache, rid libfossil.FslID, mf *manifestFiles) {
	cache.SetDerived(filesKey(rid), mf)
}

// manifestFilesAt returns the file views of d, the parse of the check-in at rid.
func manifestFilesAt(q db.Querier, cache *content.Cache, rid libfossil.FslID, d *deck.Deck) (*manifestFiles, error) {
	if v, ok := cache.Derived(filesKey(rid)); ok {
		return v.(*manifestFiles), nil
	}
	mf, err := loadManifestFiles(q, cache, d)
	if err != nil {
		return nil, err
	}
	rememberFiles(cache, rid, mf)
	return mf, nil
}

// baselineRid resolves a delta's baseline hash to its available rid. Content
// is only ever added, so an available baseline stays available and the chain
// walk behind that verdict is kept with the cache.
func baselineRid(q db.Querier, cache *content.Cache, uuid string) (libfossil.FslID, bool) {
	if v, ok := cache.Derived(baselineKey(uuid)); ok {
		return v.(libfossil.FslID), true
	}
	rid, ok := content.AvailableByUUID(q, uuid)
	if ok {
		cache.SetDerived(baselineKey(uuid), rid)
	}
	return rid, ok
}
