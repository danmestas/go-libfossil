package sync

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// TestConeCloneOfAFossilWrittenStoreSendsDeltas cone-clones a store as
// fossil writes it, with no record of which blobs the crosslinker found to be
// no artifact, whose in-cone file is large and has many versions, each a
// delta of the next. The walk sends each version as a delta, its base ahead
// of it, when the store's classification keeps the base; classifying such a
// store read every file version and exceeded its byte budget, so every
// version went out whole and a round held two. A cone clone of
// fossil-scm.org's repository took about 390 rounds where a dense clone
// takes 8.
func TestConeCloneOfAFossilWrittenStoreSendsDeltas(t *testing.T) {
	const (
		versions  = 66
		fileBytes = 8 << 20 // versions*fileBytes exceeds classification's expanded budget
	)
	origin := setupSyncTestRepo(t)
	// SHA1 names: hashing 528 MB of versions under SHA3 cost seconds here.
	if _, err := origin.DB().Exec("REPLACE INTO config(name,value,mtime) VALUES('hash-policy','0',0)"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	for n := 0; buf.Len() < fileBytes; n++ {
		fmt.Fprintf(&buf, "line %07d of a large in-cone file whose every version is kept\n", n)
	}
	text := buf.Bytes()
	when := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var parent string
	var files []libfossil.FslID
	for i := range versions {
		copy(text[i*64:], fmt.Sprintf("version %04d", i)) // each version differs from the last
		frid, fuuid, err := blob.Store(origin.DB(), text)
		if err != nil {
			t.Fatal(err)
		}
		_, out, err := blob.Store(origin.DB(), fmt.Appendf(nil, "outside %d\n", i))
		if err != nil {
			t.Fatal(err)
		}
		d := &deck.Deck{Type: deck.Checkin, C: fmt.Sprintf("version %d", i), U: deck.User("u"),
			D: when.Add(time.Duration(i) * time.Hour),
			F: []deck.FileCard{{Name: "doc/out.txt", UUID: out}, {Name: "src/big.c", UUID: fuuid}}}
		if parent != "" {
			d.P = []string{parent}
		}
		data, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, parent, err = blob.Store(origin.DB(), data); err != nil {
			t.Fatal(err)
		}
		files = append(files, frid)
	}
	err := origin.WithTx(func(tx *db.Tx) error {
		for i := 0; i+1 < versions; i++ {
			if _, err := content.Deltify(tx, files[i], files[i+1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(origin); err != nil {
		t.Fatal(err)
	}
	if _, err := origin.DB().Exec("DROP TABLE IF EXISTS crosslink_nonartifact_v2"); err != nil {
		t.Fatal(err)
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	tr := &cfileCounter{r: origin, sent: map[string]int{}}
	clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"),
		tr, CloneOpts{StoreCone: c})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	t.Logf("%d rounds, %d artifacts", tr.rounds, len(tr.sent))
	// The versions' deltas and one whole text fit a round; the clone ends
	// with the round that carries the clone cursor's end.
	if tr.rounds > 3 {
		t.Fatalf("cone clone took %d rounds, want at most 3", tr.rounds)
	}
}
