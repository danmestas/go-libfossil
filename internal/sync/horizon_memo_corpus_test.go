package sync

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestCorpusHorizonPullAfterPush times what draft-fossil-horizon-00 §6.2
// says the classification memo is for: a horizon pull that follows a
// one-artifact push to a dense origin. It is skipped unless LIBFOSSIL_CORPUS
// names a repository; it works on a copy.
func TestCorpusHorizonPullAfterPush(t *testing.T) {
	corpus := os.Getenv("LIBFOSSIL_CORPUS")
	if corpus == "" {
		t.Skip("set LIBFOSSIL_CORPUS to a .fossil repository to run")
	}
	work := filepath.Join(t.TempDir(), "corpus.fossil")
	if err := corpusCopy(work, corpus); err != nil {
		t.Fatalf("copy corpus: %v", err)
	}
	r, err := repo.Open(work)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	if err := auth.UpdateCaps(r.DB(), "nobody", "gijoryz"); err != nil {
		t.Fatal(err)
	}
	var pc, tip string
	if err := r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc); err != nil {
		t.Fatal(err)
	}
	if err := r.DB().QueryRow(`SELECT b.uuid FROM event e JOIN blob b ON b.rid=e.objid
		WHERE e.type='ci' ORDER BY e.mtime DESC LIMIT 1`).Scan(&tip); err != nil {
		t.Fatal(err)
	}
	c := corpusAncestor(t, r, tip, 1000)
	when := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		depth cone.HorizonDepth
	}{{"content", cone.DepthContent}, {"structural", cone.DepthStructural}} {
		h, err := cone.NewHorizon(c, cone.RuleDescendants, tc.depth)
		if err != nil {
			t.Fatal(err)
		}
		hc, err := cone.NewWithHorizon(nil, nil, h)
		if err != nil {
			t.Fatal(err)
		}
		pull := func(cursor int64) (time.Duration, int64) {
			t.Helper()
			cards := []xfer.Card{&xfer.PullCard{ServerCode: "corpus", ProjectCode: pc}}
			cards = append(cards, conePragmas(hc, noSeed)...)
			if cursor > 0 {
				cards = append(cards, cursorCard(cursor))
			}
			start := time.Now()
			resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
			if err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(start)
			for _, p := range findCards[*xfer.PragmaCard](resp) {
				if p.Name == horizonCursorPragma {
					if n, ok := parseCursor(p); ok {
						return elapsed, n
					}
				}
			}
			return elapsed, cursor
		}
		cold, cursor := pull(0)
		warm, cursor := pull(cursor)
		when = when.Add(time.Hour)
		d := &deck.Deck{Type: deck.Checkin, C: "memo " + tc.name, D: when, U: deck.User("u"), P: []string{tip}}
		data, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
			&xfer.PushCard{ServerCode: "corpus", ProjectCode: pc},
			&xfer.FileCard{UUID: corpusHash(data, tip), Content: data},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if errs := findCards[*xfer.ErrorCard](resp); len(errs) > 0 {
			t.Fatalf("push: %s", errs[0].Message)
		}
		pushed := time.Since(start)
		after, _ := pull(cursor)
		t.Logf("%s horizon at %s: cold pull %s, unchanged pull %s, one-artifact push %s, pull after the push %s",
			tc.name, c[:10], cold, warm, pushed, after)
		tip = corpusHash(data, tip)
	}
}

// corpusAncestor walks n primary parents back from uuid, stopping at a root.
func corpusAncestor(t *testing.T, r *repo.Repo, uuid string, n int) string {
	t.Helper()
	for i := 0; i < n; i++ {
		var parent string
		err := r.DB().QueryRow(`SELECT p.uuid FROM plink JOIN blob c ON c.rid=plink.cid
			JOIN blob p ON p.rid=plink.pid WHERE c.uuid=? AND plink.isprim`, uuid).Scan(&parent)
		if err != nil {
			return uuid
		}
		uuid = parent
	}
	return uuid
}

func corpusCopy(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// corpusHash names data under the hash of like, as the store names its artifacts.
func corpusHash(data []byte, like string) string {
	if len(like) == 40 {
		return hash.SHA1(data)
	}
	return hash.SHA3(data)
}
