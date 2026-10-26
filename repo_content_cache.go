package libfossil

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// contentCache is the cache SetContentCache attached, valid for the store as
// it stood after writes finished writes (db.DB.Writes).
//
// Content named by hash never changes, but a write can take it away: a purge
// deletes the row, and SQLite gives a deleted newest rid to the next blob
// stored; compaction and horizon or cone changes turn held blobs into
// phantoms. What the cache keeps under a rid, and the manifests parsed from
// it, are therefore good only until the next write. Rather than have every
// such write know about the cache, a read that sees the write count moved
// replaces the cache with an empty one. Reads in flight keep the cache they
// started with, so nothing they store lands in its successor.
type contentCache struct {
	cache  *content.Cache
	bytes  int64
	writes int64
}

// SetContentCache attaches a cache of expanded artifact content, bounded by
// bytes, that ReadFile, ReadFileAt, ReadBlob, ListFiles, ListFilesIn, Diff and
// Annotate share. It replaces any cache already attached; bytes <= 0 detaches
// it. The default is no cache. It is safe to call while other goroutines read.
//
// The bound covers expanded content; the parses of the few check-in
// manifests read most recently are kept beside it. Any write through this
// Repo empties the cache. A write by another process, or through another
// Repo opened on the same file, does not: attach a cache only where this
// Repo is the store's one writer, or where no other writer purges, compacts
// or narrows it.
func (r *Repo) SetContentCache(bytes int64) {
	if bytes <= 0 {
		r.cache.Store(nil)
		return
	}
	r.cache.Store(newContentCache(bytes, r.DB().Writes()))
}

func newContentCache(bytes, writes int64) *contentCache {
	if bytes <= 0 {
		panic("libfossil.newContentCache: bytes must be > 0")
	}
	return &contentCache{cache: content.NewCache(bytes), bytes: bytes, writes: writes}
}

// liveCache returns the cache a read may use now, or nil to read uncached.
// A cache that predates the latest finished write is replaced by an empty
// one first. Losing that swap to another reader or to SetContentCache leaves
// this read uncached, which is always correct.
func (r *Repo) liveCache() *content.Cache {
	cc := r.cache.Load()
	if cc == nil {
		return nil
	}
	writes := r.DB().Writes()
	if cc.writes == writes {
		return cc.cache
	}
	fresh := newContentCache(cc.bytes, writes)
	if !r.cache.CompareAndSwap(cc, fresh) {
		return nil
	}
	return fresh.cache
}

// ReadBlob returns the content of the artifact named uuid, a full SHA1 or
// SHA3-256 hash in lowercase hex, such as the UUID ListFiles reports for a
// file. An unknown hash returns ErrArtifactNotFound (wrapped). An artifact
// the store knows but does not hold, such as one elided below a horizon or
// outside a sparse cone, fails as ReadFile of a path naming it does.
func (r *Repo) ReadBlob(uuid string) ([]byte, error) {
	if uuid == "" {
		return nil, fmt.Errorf("libfossil: read blob: uuid is required")
	}
	if !hash.IsValidHash(uuid) {
		return nil, fmt.Errorf("libfossil: read blob: %q is not a full lowercase artifact hash", uuid)
	}
	if _, known := blob.Exists(r.DB(), uuid); !known {
		return nil, fmt.Errorf("libfossil: read blob %s: %w", uuid, ErrArtifactNotFound)
	}
	data, err := readHeld(r, r.liveCache(), uuid)
	if err != nil {
		return nil, fmt.Errorf("libfossil: read blob: %w", err)
	}
	return data, nil
}

// readHeld expands the artifact named uuid through cache (nil reads uncached)
// once the store proves it holds it, as every read by hash here does.
func readHeld(r *Repo, cache *content.Cache, uuid string) ([]byte, error) {
	rid, err := content.RequireAvailableByUUID(r.DB(), uuid)
	if err != nil {
		return nil, err
	}
	return cache.Expand(r.DB(), rid)
}
