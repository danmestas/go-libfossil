package sync

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// cfileCounter is a transport to an in-process handler that counts the
// cfile cards each artifact went out in.
type cfileCounter struct {
	r      *repo.Repo
	sent   map[string]int
	rounds int
}

func (c *cfileCounter) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	resp, err := HandleSync(ctx, c.r, req)
	if err != nil {
		return nil, err
	}
	c.rounds++
	for _, card := range resp.Cards {
		if f, ok := card.(*xfer.CFileCard); ok {
			c.sent[f.UUID]++
		}
	}
	return resp, nil
}

// TestCloneSendsAChainOnce has an origin hold a file's history as fossil
// does, each version a delta of the next, with other rows between the
// versions so the clone takes several rounds. Each version's row reached in
// a later round must not send the chain above it again: its client already
// holds it. Sending it again made a dense clone of the Fossil repository
// carry each history about five times over, every copy verified again.
func TestCloneSendsAChainOnce(t *testing.T) {
	origin := setupSyncTestRepo(t)
	rng := rand.New(rand.NewSource(1))
	const versions = 6
	var rids []libfossil.FslID
	text := []byte("a file whose history is one chain of reverse deltas\n")
	for i := range versions {
		text = append(text, fmt.Sprintf("line %d of the history\n", i)...)
		rid, _, err := blob.Store(origin.DB(), text)
		if err != nil {
			t.Fatal(err)
		}
		rids = append(rids, rid)
		filler := make([]byte, DefaultCloneBatchBytes/3)
		rng.Read(filler)
		if _, _, err := blob.Store(origin.DB(), filler); err != nil {
			t.Fatal(err)
		}
	}
	err := origin.WithTx(func(tx *db.Tx) error {
		for i := 0; i+1 < versions; i++ {
			if _, err := content.Deltify(tx, rids[i], rids[i+1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	tr := &cfileCounter{r: origin, sent: map[string]int{}}
	clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"),
		tr, CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	if tr.rounds < 3 {
		t.Fatalf("clone took %d rounds; the fixture needs several", tr.rounds)
	}
	var want int
	if err := origin.DB().QueryRow("SELECT count(*) FROM blob WHERE size>=0").Scan(&want); err != nil {
		t.Fatal(err)
	}
	if len(tr.sent) != want {
		t.Errorf("%d artifacts went out, want %d", len(tr.sent), want)
	}
	for uuid, n := range tr.sent {
		if n != 1 {
			t.Errorf("%s went out %d times", uuid[:10], n)
		}
	}
	for _, rid := range rids {
		var uuid string
		if err := origin.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			t.Fatal(err)
		}
		cr, ok := blob.Exists(clone.DB(), uuid)
		if !ok {
			t.Fatalf("clone lacks %s", uuid[:10])
		}
		if _, err := content.Expand(clone.DB(), cr); err != nil {
			t.Errorf("clone's %s: %v", uuid[:10], err)
		}
	}
}
