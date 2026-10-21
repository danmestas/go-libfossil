package libfossil

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func requiredCheckoutMissingPayload(t *testing.T) (requiredPublicFixture, *Checkout, string) {
	t.Helper()
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
	return f, co, uuid
}

func requiredRequestsPayload(t *testing.T, payload []byte, uuid string) bool {
	t.Helper()
	message, err := xfer.Decode(payload, xfer.ContentTypeCompressed)
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range message.Cards {
		if gimme, ok := card.(*xfer.GimmeCard); ok && gimme.UUID == uuid {
			return true
		}
	}
	return false
}

// A carried in-cone file is consumed by the writer even when the store cone
// excludes it. Recover exactly that payload without changing retention policy.
func TestCheckinContextRequiredInConePayload(t *testing.T) {
	for _, narrowStore := range []bool{false, true} {
		f, co, uuid := requiredCheckoutMissingPayload(t)
		if narrowStore {
			c, err := NewCone([]string{"f0003"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.target.SetStoreCone(c); err != nil {
				t.Fatal(err)
			}
		}
		before, err := f.target.StoreCone()
		if err != nil {
			t.Fatal(err)
		}
		requested := false
		peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
			requested = requested || requiredRequestsPayload(t, payload, uuid)
			return f.source.HandleSync(ctx, payload)
		})
		if _, _, err := co.CheckinContext(context.Background(), CheckoutCommitOpts{
			User: "test", AllowEmpty: true, Files: []string{"f0003"},
		}, RequiredContentOpts{Peer: peer}); err != nil {
			t.Fatal(err)
		}
		if _, held := content.AvailableByUUID(f.target.inner.DB(), uuid); !held || !requested {
			t.Fatal("necessary carried payload was not explicitly recovered")
		}
		after, err := f.target.StoreCone()
		if err != nil || after == nil || after.Digest() != before.Digest() {
			t.Fatalf("checkin recovery changed store policy: %v", err)
		}
	}
}

// A selected file whose repository bytes are absent is rebuilt from its live
// materialized bytes. Recovery must not fetch a payload the writer replaces.
func TestCheckinContextDoesNotFetchSelectedPayload(t *testing.T) {
	f, co, uuid := requiredCheckoutMissingPayload(t)
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		if requiredRequestsPayload(t, payload, uuid) {
			t.Fatal("selected payload was unnecessarily requested")
		}
		return f.source.HandleSync(ctx, payload)
	})
	if _, _, err := co.CheckinContext(context.Background(), CheckoutCommitOpts{
		User: "test", AllowEmpty: true, Files: []string{"f0000"},
	}, RequiredContentOpts{Peer: peer}); err != nil {
		t.Fatal(err)
	}
}
