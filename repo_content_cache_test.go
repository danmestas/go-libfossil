package libfossil

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/shun"
)

// newReadBlobRepo commits three revisions of a few files and returns the
// repository with the check-in rids, oldest first.
func newReadBlobRepo(t *testing.T) (*Repo, []int64) {
	t.Helper()
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	var rids []int64
	parent := int64(0)
	for rev := 0; rev < 3; rev++ {
		var files []FileToCommit
		for _, name := range []string{"a.txt", "b.txt", "dir/c.txt", "dir/d.txt"} {
			body := fmt.Sprintf("%s line one\nshared\n%s rev %d\n", name, name, rev)
			files = append(files, FileToCommit{Name: name, Content: []byte(body)})
		}
		rid, _, err := r.Commit(CommitOpts{User: "test", ParentID: parent, Files: files})
		if err != nil {
			t.Fatal(err)
		}
		rids = append(rids, rid)
		parent = rid
	}
	return r, rids
}

// cacheStats is the attached cache's statistics; the test fails if none is.
func cacheStats(t *testing.T, r *Repo) content.CacheStats {
	t.Helper()
	c := r.liveCache()
	if c == nil {
		t.Fatal("no content cache attached")
	}
	return c.Stats()
}

// ReadBlob of a hash ListFiles names returns what ReadFile of the path does,
// with and without a content cache.
func TestReadBlobMatchesReadFile(t *testing.T) {
	r, rids := newReadBlobRepo(t)
	for _, cached := range []bool{false, true} {
		if cached {
			r.SetContentCache(1 << 20)
		}
		for _, rid := range rids {
			files, err := r.ListFiles(rid)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				want, err := r.ReadFile(rid, f.Name)
				if err != nil {
					t.Fatal(err)
				}
				got, err := r.ReadBlob(f.UUID)
				if err != nil {
					t.Fatalf("cached=%v ReadBlob(%s): %v", cached, f.Name, err)
				}
				if string(got) != string(want) {
					t.Fatalf("cached=%v %s: ReadBlob %q, ReadFile %q", cached, f.Name, got, want)
				}
			}
		}
	}
}

// ReadBlob refuses an empty or malformed hash before touching the store, and
// names an unknown hash as an unknown artifact.
func TestReadBlobRejects(t *testing.T) {
	r, _ := newReadBlobRepo(t)
	for _, uuid := range []string{"", "abc", strings.Repeat("A", 40), strings.Repeat("g", 64)} {
		if _, err := r.ReadBlob(uuid); err == nil {
			t.Fatalf("ReadBlob(%q) succeeded", uuid)
		}
	}
	unknown := hash.SHA1([]byte("never committed\n"))
	if _, err := r.ReadBlob(unknown); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("ReadBlob(unknown) = %v, want ErrArtifactNotFound", err)
	}
}

// Below a horizon ReadBlob fails as ReadFile does: the blob is an elided
// phantom, and both say so the same way, with and without a cache.
func TestReadBlobBelowHorizon(t *testing.T) {
	f := newHorizonFixture(t)
	hc, err := NewHorizonCone(nil, nil, Horizon{Checkin: f.c2})
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "t.fossil"),
		f.peer, CloneOpts{StoreCone: hc, PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	rid1, err := target.ResolveVersion(f.c1)
	if err != nil {
		t.Fatal(err)
	}
	old := hash.SHA1([]byte("old a\n"))
	for _, cached := range []bool{false, true} {
		if cached {
			target.SetContentCache(1 << 20)
		}
		_, fileErr := target.ReadFile(rid1, "a")
		_, blobErr := target.ReadBlob(old)
		if !errors.Is(fileErr, ErrBelowHorizon) || !strings.Contains(fileErr.Error(), f.c2) {
			t.Fatalf("cached=%v below-horizon file read: %v, want ErrBelowHorizon naming %s", cached, fileErr, f.c2)
		}
		if blobErr == nil || !strings.Contains(blobErr.Error(), "elided") {
			t.Fatalf("cached=%v below-horizon blob read: %v", cached, blobErr)
		}
		for _, err := range []error{fileErr, blobErr} {
			if !strings.Contains(err.Error(), "deepen") {
				t.Fatalf("cached=%v below-horizon read names no deepen: %v", cached, err)
			}
		}
		if got, err := target.ReadBlob(hash.SHA1([]byte("shared b\n"))); err != nil ||
			string(got) != "shared b\n" {
			t.Fatalf("cached=%v held blob: %q, %v", cached, got, err)
		}
	}
}

// With a cache, a second path at the same check-in is served from the parse
// the first read kept: no manifest is expanded or parsed again, and the only
// miss is the new file's own blob.
func TestContentCacheReusesManifest(t *testing.T) {
	r, rids := newReadBlobRepo(t)
	r.SetContentCache(1 << 20)
	tip := rids[len(rids)-1]
	if _, err := r.ReadFile(tip, "a.txt"); err != nil {
		t.Fatal(err)
	}
	before := cacheStats(t, r)
	if _, err := r.ReadFileAt("tip", "dir/c.txt"); err != nil {
		t.Fatal(err)
	}
	after := cacheStats(t, r)
	if after.DerivedHits <= before.DerivedHits {
		t.Fatalf("manifest parse not reused: derived hits %d -> %d",
			before.DerivedHits, after.DerivedHits)
	}
	if got := after.Misses - before.Misses; got != 1 {
		t.Fatalf("second read missed %d times, want 1 (its own blob)", got)
	}
	if _, err := r.ListFiles(tip); err != nil {
		t.Fatal(err)
	}
	if got := cacheStats(t, r).Misses - after.Misses; got != 0 {
		t.Fatalf("ListFiles of a read check-in missed %d times", got)
	}
}

// Diff and Annotate answer the same with a cache as without.
func TestContentCacheDiffAnnotateUnchanged(t *testing.T) {
	r, rids := newReadBlobRepo(t)
	read := func() ([]DiffEntry, []AnnotatedLine) {
		d, err := r.Diff(rids[0], rids[2], "")
		if err != nil {
			t.Fatal(err)
		}
		a, err := r.Annotate(AnnotateOpts{FilePath: "dir/c.txt", StartRID: rids[2]})
		if err != nil {
			t.Fatal(err)
		}
		return d, a
	}
	wantDiff, wantAnn := read()
	r.SetContentCache(1 << 20)
	for range 2 {
		gotDiff, gotAnn := read()
		if !reflect.DeepEqual(gotDiff, wantDiff) || !reflect.DeepEqual(gotAnn, wantAnn) {
			t.Fatal("cached Diff or Annotate differs from uncached")
		}
	}
	if cacheStats(t, r).Hits == 0 {
		t.Fatal("Diff and Annotate did not use the cache")
	}
}

// Compaction elides blobs the cache already holds. A read after it refuses as
// an uncached read would, rather than serving the bytes the cache kept.
func TestContentCacheNotStaleAfterCompaction(t *testing.T) {
	f := newCompactFixture(t)
	r := f.target
	r.SetContentCache(1 << 20)
	files, err := r.ListFiles(f.parent)
	if err != nil {
		t.Fatal(err)
	}
	uuid := ""
	for _, file := range files {
		if file.Name == "zzz/b" {
			uuid = file.UUID
		}
	}
	if got, err := r.ReadFile(f.parent, "zzz/b"); err != nil || string(got) != "outside\n" {
		t.Fatalf("before compaction: %q, %v", got, err)
	}
	if _, err := r.ReadBlob(uuid); err != nil {
		t.Fatal(err)
	}
	narrow, err := NewCone([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetStoreCone(narrow); err != nil {
		t.Fatal(err)
	}
	if n, err := r.CompactStore(context.Background()); err != nil || n == 0 {
		t.Fatalf("compaction = %d, %v", n, err)
	}
	if got, err := r.ReadFile(f.parent, "zzz/b"); err == nil {
		t.Fatalf("ReadFile served elided content %q", got)
	}
	if got, err := r.ReadBlob(uuid); err == nil {
		t.Fatalf("ReadBlob served elided content %q", got)
	}
	if got, err := r.ReadFile(f.parent, "src/a"); err != nil || string(got) != "inside\n" {
		t.Fatalf("in-cone read after compaction: %q, %v", got, err)
	}
}

// A shunned and purged artifact is gone, and the cache that held it does not
// bring it back: not the purged check-in's manifest, which a read through it
// would otherwise still resolve paths against, and not under its rid, which
// SQLite hands to the next blob stored once the newest row is deleted.
func TestContentCacheNotStaleAfterPurge(t *testing.T) {
	r, rids := newReadBlobRepo(t)
	r.SetContentCache(1 << 20)
	tip := rids[len(rids)-1]
	var newest int64
	if err := r.DB().QueryRow("SELECT max(rid) FROM blob").Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if newest != tip {
		t.Fatalf("newest blob is rid %d, want the tip manifest %d", newest, tip)
	}
	tipUUID, err := r.UUIDFromRID(tip)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadFile(tip, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadBlob(tipUUID); err != nil {
		t.Fatal(err)
	}
	if err := shun.Add(r.DB(), tipUUID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := shun.Purge(r.DB()); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ReadBlob(tipUUID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("ReadBlob after purge = %q, %v; want ErrArtifactNotFound", got, err)
	}
	if got, err := r.ReadFile(tip, "a.txt"); err == nil {
		t.Fatalf("ReadFile through a purged manifest served %q", got)
	}
	body := []byte("stored after the purge\n")
	next, nextUUID, err := r.Commit(CommitOpts{User: "test", ParentID: rids[1],
		Files: []FileToCommit{{Name: "fresh.txt", Content: body}}})
	if err != nil {
		t.Fatal(err)
	}
	files, err := r.ListFiles(next)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		got, err := r.ReadBlob(f.UUID)
		if err != nil || hash.ContentHash(got, f.UUID) != f.UUID {
			t.Fatalf("ReadBlob(%s) = %q, %v: not the named content", f.Name, got, err)
		}
	}
	got, err := r.ReadBlob(nextUUID)
	if err != nil || hash.ContentHash(got, nextUUID) != nextUUID {
		t.Fatalf("ReadBlob(new manifest) = %v: not the named content", err)
	}
}

// The cache holds no more expanded content than it was given, however many
// distinct blobs are read through it.
func TestContentCacheBounded(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var files []FileToCommit
	for i := range 64 {
		body := strings.Repeat(fmt.Sprintf("file %d\n", i), 128)
		files = append(files, FileToCommit{Name: fmt.Sprintf("f%02d", i), Content: []byte(body)})
	}
	rid, _, err := r.Commit(CommitOpts{User: "test", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	const budget = 8 << 10
	r.SetContentCache(budget)
	for _, f := range files {
		if _, err := r.ReadFile(rid, f.Name); err != nil {
			t.Fatal(err)
		}
		if s := cacheStats(t, r); s.Size > budget || s.MaxSize != budget {
			t.Fatalf("cache holds %d bytes of a %d budget (max %d)", s.Size, budget, s.MaxSize)
		}
	}
	r.SetContentCache(0)
	if r.liveCache() != nil {
		t.Fatal("SetContentCache(0) left a cache attached")
	}
	if _, err := r.ReadFile(rid, "f00"); err != nil {
		t.Fatal(err)
	}
}

// Reads may run while the cache is attached, replaced and detached and while
// the repository is written; run under -race.
func TestContentCacheConcurrentReads(t *testing.T) {
	r, rids := newReadBlobRepo(t)
	tip := rids[len(rids)-1]
	files, err := r.ListFiles(tip)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				f := files[(g+i)%len(files)]
				a, err := r.ReadFile(tip, f.Name)
				if err != nil {
					errs <- err
					return
				}
				b, err := r.ReadBlob(f.UUID)
				if err != nil || string(a) != string(b) {
					errs <- fmt.Errorf("%s: ReadBlob %q, %v; ReadFile %q", f.Name, b, err, a)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 20 {
			r.SetContentCache(int64(i%3) << 16)
		}
		if _, _, err := r.Commit(CommitOpts{User: "test", ParentID: tip,
			Files: []FileToCommit{{Name: "new.txt", Content: []byte("new\n")}}}); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
