package seedcheck_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
	"github.com/danmestas/go-libfossil/simio"
)

func dropRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "r.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// seedDeck is a seed by this implementation's own label.
func seedDeck(t *testing.T) (*deck.Deck, []byte) {
	t.Helper()
	d := &deck.Deck{Type: deck.Control, D: time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC), U: deck.User(seedcheck.Label),
		T: []deck.TagCard{{Type: deck.TagSingleton, Name: "alpha", UUID: "0123456789abcdef0123456789abcdef01234567"}}}
	data, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return d, data
}

// onto stores target as a delta on the row at src and returns its rid.
func onto(t *testing.T, r *repo.Repo, src libfossil.FslID, base, target []byte) libfossil.FslID {
	t.Helper()
	d, err := delta.Create(base, target)
	if err != nil {
		t.Fatal(err)
	}
	rid, err := blob.StoreDeltaRaw(r.DB(), hash.SHA1(target), d, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rid
}

// assertDropped checks that the seed at rid holds no content, is no phantom
// any request would ask for, is no delta, and stays refused.
func assertDropped(t *testing.T, r *repo.Repo, rid libfossil.FslID, uuid string) {
	t.Helper()
	var size int64
	var held []byte
	if err := r.DB().QueryRow("SELECT size, content FROM blob WHERE rid=?", rid).Scan(&size, &held); err != nil {
		t.Fatalf("the dropped seed's rid is not held: %v", err)
	}
	if size != -1 || held != nil {
		t.Fatalf("the dropped seed keeps content: size=%d, %d content bytes", size, len(held))
	}
	var n int
	r.DB().QueryRow("SELECT count(*) FROM phantom WHERE rid=?", rid).Scan(&n)
	if n != 0 {
		t.Fatal("the dropped seed is a phantom a request would ask for")
	}
	r.DB().QueryRow("SELECT count(*) FROM delta WHERE rid=? OR srcid=?", rid, rid).Scan(&n)
	if n != 0 {
		t.Fatalf("%d delta rows still name the dropped seed", n)
	}
	if refused, err := seedcheck.IsRefusedUUID(r.DB(), uuid); err != nil || !refused {
		t.Fatalf("the dropped seed is not refused (%v)", err)
	}
	if withheld, err := seedcheck.IsWithheld(r.DB(), rid); err != nil || !withheld {
		t.Fatalf("the dropped seed is not withheld (%v)", err)
	}
}

// assertExpands checks that rid still reads back as want.
func assertExpands(t *testing.T, r *repo.Repo, rid libfossil.FslID, want []byte) {
	t.Helper()
	got, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatalf("rid=%d no longer expands: %v", rid, err)
	}
	if string(got) != string(want) {
		t.Fatalf("rid=%d expands to other bytes", rid)
	}
}

// A seed completed by a later fill is dropped (§5.2.3): its row keeps its
// rid, so the deltas built on it are rewritten as full text first and still
// read back, but its content is gone.
func TestJudgeFilledDropsLateSeed(t *testing.T) {
	r := dropRepo(t)
	_, seed := seedDeck(t)
	base := []byte("the basis a later round supplies\n")
	src, err := blob.StorePhantom(r.DB(), hash.SHA3(base))
	if err != nil {
		t.Fatal(err)
	}
	seedRid := onto(t, r, src, base, seed)
	child := append([]byte("built on the seed\n"), seed...)
	childRid := onto(t, r, seedRid, seed, child)
	grandchild := append([]byte("built on its dependent\n"), child...)
	grandRid := onto(t, r, childRid, child, grandchild)
	for _, rid := range []libfossil.FslID{seedRid, childRid, grandRid} {
		if err := seedcheck.MarkPending(r.DB(), rid); err != nil {
			t.Fatal(err)
		}
	}
	filled, _, err := blob.Store(r.DB(), base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedcheck.JudgeFilled(r.DB(), []libfossil.FslID{filled}); err != nil {
		t.Fatal(err)
	}
	assertDropped(t, r, seedRid, hash.SHA1(seed))
	assertExpands(t, r, childRid, child)
	assertExpands(t, r, grandRid, grandchild)
	for _, rid := range []libfossil.FslID{childRid, grandRid} {
		if withheld, err := seedcheck.IsWithheld(r.DB(), rid); err != nil || withheld {
			t.Fatalf("rid=%d, judged not a seed, is still withheld (%v)", rid, err)
		}
	}
}

// The link step's verdict drops the seed the same way.
func TestJudgeDropsSeedAtLinkStep(t *testing.T) {
	r := dropRepo(t)
	d, seed := seedDeck(t)
	seedRid, uuid, err := blob.Store(r.DB(), seed)
	if err != nil {
		t.Fatal(err)
	}
	child := append([]byte("built on the seed\n"), seed...)
	childRid := onto(t, r, seedRid, seed, child)
	refused, err := seedcheck.Judge(r.DB(), seedcheck.Tables{}, seedRid, d)
	if err != nil || !refused {
		t.Fatalf("the seed was not refused at the link step (%v)", err)
	}
	assertDropped(t, r, seedRid, uuid)
	assertExpands(t, r, childRid, child)
}

// A drop that fails partway leaves nothing applied: the seed keeps its
// content and is not refused, and the delta rewritten before the failure is
// a delta on it again. A caller that logs the failure and commits (the
// after-dephantomize cascade) commits no half-dropped seed.
func TestDropIsAtomic(t *testing.T) {
	t.Run("pool", func(t *testing.T) {
		testDropIsAtomic(t, func(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) error {
			_, err := seedcheck.Judge(r.DB(), seedcheck.Tables{}, rid, d)
			return err
		})
	})
	// In a transaction the caller commits after logging the failure.
	t.Run("transaction", func(t *testing.T) {
		testDropIsAtomic(t, func(r *repo.Repo, rid libfossil.FslID, d *deck.Deck) error {
			var judged error
			err := r.WithTx(func(tx *db.Tx) error {
				_, judged = seedcheck.Judge(tx, seedcheck.Tables{}, rid, d)
				return nil
			})
			return errors.Join(judged, err)
		})
	})
}

func testDropIsAtomic(t *testing.T, judge func(*repo.Repo, libfossil.FslID, *deck.Deck) error) {
	r := dropRepo(t)
	d, seed := seedDeck(t)
	seedRid, uuid, err := blob.Store(r.DB(), seed)
	if err != nil {
		t.Fatal(err)
	}
	good := append([]byte("built on the seed\n"), seed...)
	goodRid := onto(t, r, seedRid, seed, good)
	bad := onto(t, r, seedRid, seed, append([]byte("whose stored bytes are damaged\n"), seed...))
	if _, err := r.DB().Exec("UPDATE blob SET content=x'00000010deadbeef' WHERE rid=?", bad); err != nil {
		t.Fatal(err)
	}
	if err := judge(r, seedRid, d); err == nil {
		t.Fatal("a drop over a damaged dependent reported no error")
	}
	var size int64
	r.DB().QueryRow("SELECT size FROM blob WHERE rid=?", seedRid).Scan(&size)
	if size < 0 {
		t.Fatal("the failed drop emptied the seed's row")
	}
	if refused, err := seedcheck.IsRefusedUUID(r.DB(), uuid); err != nil || refused {
		t.Fatalf("the failed drop refused the seed (%v)", err)
	}
	if src, err := content.DeltaSource(r.DB(), goodRid); err != nil || src != seedRid {
		t.Fatalf("the dependent rewritten before the failure kept its rewrite: source %d (%v)", src, err)
	}
	assertExpands(t, r, goodRid, good)
}
