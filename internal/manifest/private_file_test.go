package manifest

import (
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
)

// A public check-in makes every file blob it names public, as fossil's commit
// does (checkin.c, content_make_public per F card): content first stored
// privately, say by a private branch, would otherwise never reach a peer
// without private access, leaving the check-in's file a phantom there. The
// test stores the content privately, commits it publicly, and checks the
// blob left the private table and is queued for the next push.
func TestCheckinMakesReusedPrivateFileBlobPublic(t *testing.T) {
	r := sha3Repo(t)
	data := []byte("shared with a private branch\n")
	if err := r.WithTx(func(tx *db.Tx) error {
		rid, _, err := blob.Store(tx, data)
		if err != nil {
			return err
		}
		return content.MakePrivate(tx, int64(rid))
	}); err != nil {
		t.Fatal(err)
	}
	_, d := commit(t, r, CheckinOpts{
		Files:   []File{{Name: "f.txt", Content: data}},
		Comment: "public",
	})
	if len(d.F) != 1 {
		t.Fatalf("F cards = %d, want 1", len(d.F))
	}
	if isPrivate(t, r, d.F[0].UUID) {
		t.Fatal("file blob stays private under a public check-in")
	}
	rid, _ := blob.Exists(r.DB(), d.F[0].UUID)
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM unsent WHERE rid=?", rid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("file blob made public but not queued for the next push")
	}
}
