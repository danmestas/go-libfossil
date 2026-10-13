package sync

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// TestDeltaMatches checks the pre-storage verification both receive sides
// share (Section 7.7): a delta against a held basis must give HASH, a delta
// that does not apply is a mismatch, and an unheld basis is not checked.
func TestDeltaMatches(t *testing.T) {
	r := setupSyncTestRepo(t)
	basis := []byte("the basis of a delta: alpha beta gamma delta epsilon\n")
	target := []byte("the target of a delta: alpha beta gamma delta epsilon zeta\n")
	_, basisName, err := blob.Store(r.DB(), basis)
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}
	d, err := delta.Create(basis, target)
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	other := hash.SHA1([]byte("some other content"))
	unheld := hash.SHA1([]byte("a basis nobody holds"))
	for _, tt := range []struct {
		name, uuid, src string
		payload         []byte
		want            bool
	}{
		{"result is HASH", hash.SHA1(target), basisName, d, true},
		{"result is SHA3 HASH", hash.SHA3(target), basisName, d, true},
		{"result is not HASH", other, basisName, d, false},
		{"delta does not apply", other, basisName, []byte("not a delta"), false},
		{"basis not held", other, unheld, d, true},
	} {
		if got := deltaMatches(r.DB(), tt.uuid, tt.src, tt.payload); got != tt.want {
			t.Errorf("%s: deltaMatches = %v, want %v", tt.name, got, tt.want)
		}
	}
}
