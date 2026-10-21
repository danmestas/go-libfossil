package libfossil

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
)

// A checkout can still hold a materialized file whose repository payload has
// become elided. An unchanged carried in-cone file is needed by the writer.
func TestCheckinContextRequiredInConePayload(t *testing.T) {
	f := newRequiredPublicFixture(t, "sha3")
	co := requiredPublicCheckout(t, f)
	var uuid string
	if err := f.source.inner.DB().QueryRow(`SELECT uuid FROM blob
		WHERE content IS NOT NULL AND rid IN
		(SELECT fid FROM mlink WHERE fnid IN
		(SELECT fnid FROM filename WHERE name='f0000')) LIMIT 1`).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.target.inner.DB().Exec(
		"UPDATE blob SET size=-1,content=NULL WHERE uuid=?", uuid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := co.CheckinContext(context.Background(), CheckoutCommitOpts{
		User: "test", AllowEmpty: true, Files: []string{"f0003"},
	}, RequiredContentOpts{Peer: f.peer}); err != nil {
		t.Fatal(err)
	}
	if _, held := content.AvailableByUUID(f.target.inner.DB(), uuid); !held {
		t.Fatal("necessary carried in-cone payload was not recovered")
	}
}
