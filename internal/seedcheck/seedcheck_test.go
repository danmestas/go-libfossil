package seedcheck_test

import (
	"fmt"
	"path/filepath"
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
