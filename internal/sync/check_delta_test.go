package sync

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// TestCheckDelta checks the pre-storage verification every receiver shares
// (Section 7.7): a delta against a held basis must give HASH, a delta that
// does not apply is a mismatch, a held basis whose bytes are not its name is
// still applied and the result checked, a held basis whose bytes cannot be
// rebuilt is a mismatch, and a basis that is not held leaves the delta
// unchecked.
func TestCheckDelta(t *testing.T) {
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
	// misnamed is a held row holding basis's bytes under another name, so
	// content.Expand refuses it; unreadable is a held row whose bytes do not
	// decompress.
	misnamed := hash.SHA1([]byte("the bytes this row should hold"))
	unreadable := hash.SHA1([]byte("the bytes this other row should hold"))
	stored, err := blob.EncodeForStorage(basis, nil)
	if err != nil {
		t.Fatalf("blob.EncodeForStorage: %v", err)
	}
	for _, row := range []struct {
		name  string
		bytes []byte
	}{{misnamed, stored}, {unreadable, []byte("\x00\x00\x00\x09not zlib")}} {
		if _, err := r.DB().Exec(
			"INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, ?, ?, 1)",
			row.name, len(basis), row.bytes); err != nil {
			t.Fatalf("insert basis %s: %v", row.name, err)
		}
	}
	other := hash.SHA1([]byte("some other content"))
	unheld := hash.SHA1([]byte("a basis nobody holds"))
	for _, tt := range []struct {
		name, uuid, src string
		payload         []byte
		want            deltaVerdict
	}{
		{"result is HASH", hash.SHA1(target), basisName, d, deltaMatch},
		{"result is SHA3 HASH", hash.SHA3(target), basisName, d, deltaMatch},
		{"result is not HASH", other, basisName, d, deltaMismatch},
		{"delta does not apply", other, basisName, []byte("not a delta"), deltaMismatch},
		{"misnamed basis, result is HASH", hash.SHA1(target), misnamed, d, deltaMatch},
		{"misnamed basis, result is not HASH", other, misnamed, d, deltaMismatch},
		{"basis does not reconstruct", other, unreadable, d, deltaMismatch},
		{"basis not held", other, unheld, d, deltaUnverifiable},
	} {
		got, rebuilt, err := newReceivePass().checkDelta(r.DB(), tt.uuid, tt.src, tt.payload)
		if err != nil {
			t.Fatalf("%s: checkDelta: %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: checkDelta = %v, want %v", tt.name, got, tt.want)
		}
		// Only a match hands back content, and then the content named HASH.
		if (rebuilt != nil) != (got == deltaMatch) {
			t.Errorf("%s: content returned %v for verdict %v", tt.name, rebuilt != nil, got)
		}
		if got == deltaMatch && !hashMatches(tt.uuid, rebuilt) {
			t.Errorf("%s: rebuilt content is not %s", tt.name, tt.uuid)
		}
	}
}
