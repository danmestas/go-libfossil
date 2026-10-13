package manifest

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
)

// storedManifest writes data as a blob and runs VerifyRCard on it. The text
// is stored as written because deck.Marshal would lowercase the R card.
func storedManifest(t *testing.T, mutate func(r string) string) error {
	t.Helper()
	r := setupTestRepo(t)
	rid, _, err := Checkin(r, CheckinOpts{
		Files: []File{{Name: "a.txt", Content: []byte("one\n")}},
		User:  "u", Comment: "c", Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	raw, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	d, err := deck.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	text := string(raw)
	bad := strings.Replace(text, "R "+d.R, "R "+mutate(d.R), 1)
	// A fresh Z card is needed for the edited text to parse and hash.
	bad = bad[:strings.LastIndex(bad, "Z ")]
	bad += "Z " + md5Hex(bad) + "\n"
	var verr error
	err = r.WithTx(func(tx *db.Tx) error {
		brid, _, err := blob.Store(tx, []byte(bad))
		if err != nil {
			return err
		}
		verr = VerifyRCard(tx, content.NewCache(1<<20), brid)
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	return verr
}

func TestVerifyRCardAcceptsLowercaseDigest(t *testing.T) {
	if err := storedManifest(t, func(r string) string { return r }); err != nil {
		t.Fatalf("VerifyRCard on a correct R card: %v", err)
	}
}

func TestVerifyRCardRejectsUppercaseDigest(t *testing.T) {
	err := storedManifest(t, strings.ToUpper)
	if err == nil || !strings.Contains(err.Error(), "R-card check: manifest R card") {
		t.Fatalf("want an R-card mismatch for an uppercase R card, got %v", err)
	}
}

func TestVerifyRCardRejectsWrongDigest(t *testing.T) {
	err := storedManifest(t, func(string) string { return strings.Repeat("0", 32) })
	if err == nil || !strings.Contains(err.Error(), "R-card check: manifest R card") {
		t.Fatalf("want an R-card mismatch for a wrong R card, got %v", err)
	}
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
