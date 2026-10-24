package seedcheck_test

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

// pending stores target as a delta on base, which the store lacks, and
// records it pending, as the push handler does.
func pending(t *testing.T, r *repo.Repo, base, target []byte) libfossil.FslID {
	t.Helper()
	src, err := blob.StorePhantom(r.DB(), hash.SHA3(base))
	if err != nil {
		t.Fatal(err)
	}
	d, err := delta.Create(base, target)
	if err != nil {
		t.Fatal(err)
	}
	rid, err := blob.StoreDeltaRaw(r.DB(), hash.SHA1(target), d, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedcheck.MarkPending(r.DB(), rid); err != nil {
		t.Fatal(err)
	}
	return rid
}

// Filling one basis judges the rows pending on it and no others, however
// many are pending elsewhere: a push cannot make every later fill pay for
// the deltas it planted.
func TestJudgeFilledExaminesOnlyItsSubtree(t *testing.T) {
	r, err := repo.Create(filepath.Join(t.TempDir(), "r.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	const planted = 200
	for i := range planted {
		pending(t, r, fmt.Appendf(nil, "unrelated basis %d\n", i), fmt.Appendf(nil, "unrelated %d\n", i))
	}
	base := []byte("the basis this fill supplies\n")
	sd := &deck.Deck{Type: deck.Control, D: time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC), U: deck.User("zit-seed"),
		T: []deck.TagCard{{Type: deck.TagSingleton, Name: seedcheck.Tag, UUID: "0123456789abcdef0123456789abcdef01234567"}}}
	seed, err := sd.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	mine := pending(t, r, base, seed)
	chained := pending(t, r, seed, append([]byte("on the seed\n"), seed...))
	filled, _, err := blob.Store(r.DB(), base)
	if err != nil {
		t.Fatal(err)
	}
	examined, err := seedcheck.JudgeFilled(r.DB(), []libfossil.FslID{filled})
	if err != nil {
		t.Fatal(err)
	}
	if examined != 1 {
		t.Fatalf("examined %d pending rows, want the 1 pending on the filled basis", examined)
	}
	if refused, err := seedcheck.IsRefusedUUID(r.DB(), hash.SHA1(seed)); err != nil || !refused {
		t.Fatalf("the readable seed was not refused (%v)", err)
	}
	withheld, err := seedcheck.Withheld(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	if !withheld[mine] || !withheld[chained] {
		t.Fatal("the refused seed, or the delta on it, is not withheld")
	}
	if len(withheld) != planted+2 {
		t.Fatalf("%d rows withheld, want the %d planted rows still pending and the seed and its dependent",
			len(withheld), planted)
	}
}

// A row that cannot be expanded does not end the batch: the fills after it
// are still judged.
func TestJudgeFilledContinuesPastUnreadableRow(t *testing.T) {
	r, err := repo.Create(filepath.Join(t.TempDir(), "r.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	broken := []byte("a basis whose dependent is corrupt\n")
	src, err := blob.StorePhantom(r.DB(), hash.SHA3(broken))
	if err != nil {
		t.Fatal(err)
	}
	// A delta made against other bytes: well formed, but its checksum fails
	// when applied to the basis that arrives.
	long := bytes.Repeat([]byte("a much longer basis than the one that arrives\n"), 8)
	target := append(slices.Clone(long), []byte("and one line more\n")...)
	wrong, err := delta.Create(long, target)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := blob.StoreDeltaRaw(r.DB(), hash.SHA1(target), wrong, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedcheck.MarkPending(r.DB(), bad); err != nil {
		t.Fatal(err)
	}
	// And one whose delta applies but fails its checksum: made against bytes
	// of the arriving basis's length that differ from it.
	twin := bytes.Repeat([]byte{'x'}, len(broken))
	twinTarget := append(slices.Clone(twin), []byte("a tail\n")...)
	checksummed, err := delta.Create(twin, twinTarget)
	if err != nil {
		t.Fatal(err)
	}
	failing, err := blob.StoreDeltaRaw(r.DB(), hash.SHA1(twinTarget), checksummed, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedcheck.MarkPending(r.DB(), failing); err != nil {
		t.Fatal(err)
	}
	base := []byte("the second fill's basis\n")
	sd := &deck.Deck{Type: deck.Control, D: time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC), U: deck.User(seedcheck.Label),
		T: []deck.TagCard{{Type: deck.TagSingleton, Name: "x", UUID: "0123456789abcdef0123456789abcdef01234567"}}}
	seed, err := sd.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	pending(t, r, base, seed)
	first, _, err := blob.Store(r.DB(), broken)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := blob.Store(r.DB(), base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedcheck.JudgeFilled(r.DB(), []libfossil.FslID{first, second}); err != nil {
		t.Fatalf("JudgeFilled: %v", err)
	}
	if refused, err := seedcheck.IsRefusedUUID(r.DB(), hash.SHA1(seed)); err != nil || !refused {
		t.Fatalf("the second fill's seed was not judged (%v)", err)
	}
	if withheld, err := seedcheck.IsWithheld(r.DB(), bad); err != nil || !withheld {
		t.Fatalf("the unreadable row is no longer withheld (%v)", err)
	}
}

// A row that fails for a reason other than an unresolvable or mismatched
// chain, a store fault such as content that does not decompress, is not
// skipped silently: JudgeFilled reports it.
func TestJudgeFilledSurfacesStoreFaults(t *testing.T) {
	r, err := repo.Create(filepath.Join(t.TempDir(), "r.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	base := []byte("a basis\n")
	rid := pending(t, r, base, []byte("a basis\nand a dependent\n"))
	if _, err := r.DB().Exec("UPDATE blob SET content=x'00000010deadbeef' WHERE rid=?", rid); err != nil {
		t.Fatal(err)
	}
	filled, _, err := blob.Store(r.DB(), base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedcheck.JudgeFilled(r.DB(), []libfossil.FslID{filled}); err == nil {
		t.Fatal("JudgeFilled skipped a row whose content does not decompress")
	}
}
