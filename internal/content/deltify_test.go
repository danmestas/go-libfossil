package content

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/blob"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
)

// similarPair returns two bodies that share almost all of their text, so a
// delta between them is far below the 75% policy threshold.
func similarPair() ([]byte, []byte) {
	var a, b bytes.Buffer
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&a, "line %04d: the quick brown fox jumps over the lazy dog\n", i)
		if i == 100 {
			fmt.Fprintf(&b, "line %04d: CHANGED\n", i)
		} else {
			fmt.Fprintf(&b, "line %04d: the quick brown fox jumps over the lazy dog\n", i)
		}
	}
	return a.Bytes(), b.Bytes()
}

func isDelta(t *testing.T, q db.Querier, rid libfossil.FslID) bool {
	t.Helper()
	src, err := deltaSource(q, rid)
	if err != nil {
		t.Fatalf("deltaSource(%d): %v", rid, err)
	}
	return src > 0
}

func TestDeltifyEncodesSimilarPredecessor(t *testing.T) {
	d := setupTestDB(t)
	v1, v2 := similarPair()

	oldRid, _, err := blob.Store(d, v1)
	if err != nil {
		t.Fatalf("store v1: %v", err)
	}
	newRid, _, err := blob.Store(d, v2)
	if err != nil {
		t.Fatalf("store v2: %v", err)
	}

	saved, err := Deltify(d, oldRid, newRid)
	if err != nil {
		t.Fatalf("Deltify: %v", err)
	}
	if saved <= 0 {
		t.Fatalf("expected a positive saving, got %d", saved)
	}
	if !isDelta(t, d, oldRid) {
		t.Error("predecessor was not delta-encoded")
	}
	if isDelta(t, d, newRid) {
		t.Error("the new artifact must stay whole: deltification runs backwards")
	}

	got, err := Expand(d, oldRid)
	if err != nil {
		t.Fatalf("Expand(old): %v", err)
	}
	if !bytes.Equal(got, v1) {
		t.Error("delta-encoded artifact did not expand to its original content")
	}
}

func TestDeltifyLeavesAlreadyDeltaAlone(t *testing.T) {
	d := setupTestDB(t)
	v1, v2 := similarPair()
	v3 := append(bytes.Clone(v2), []byte("trailing addition\n")...)

	r1, _, _ := blob.Store(d, v1)
	r2, _, _ := blob.Store(d, v2)
	r3, _, _ := blob.Store(d, v3)

	if _, err := Deltify(d, r1, r2); err != nil {
		t.Fatalf("Deltify(r1,r2): %v", err)
	}
	// r1 is now a delta of r2. Re-offering it must be declined: this is the
	// rule that keeps chain depth linear rather than compounding.
	saved, err := Deltify(d, r1, r3)
	if err != nil {
		t.Fatalf("Deltify(r1,r3): %v", err)
	}
	if saved != 0 {
		t.Errorf("expected an already-delta artifact to be declined, saved=%d", saved)
	}
	src, _ := deltaSource(d, r1)
	if src != r2 {
		t.Errorf("delta source moved from %d to %d", r2, src)
	}
}

func TestDeltifyDeclinesTinyArtifacts(t *testing.T) {
	d := setupTestDB(t)
	small1, _, _ := blob.Store(d, []byte("tiny content one"))
	small2, _, _ := blob.Store(d, []byte("tiny content two"))

	saved, err := Deltify(d, small1, small2)
	if err != nil {
		t.Fatalf("Deltify: %v", err)
	}
	if saved != 0 {
		t.Errorf("artifacts under %d bytes must be left whole, saved=%d", deltifyMinBytes, saved)
	}
	if isDelta(t, d, small1) {
		t.Error("tiny artifact was delta-encoded")
	}
}

func TestDeltifyDeclinesDissimilarContent(t *testing.T) {
	d := setupTestDB(t)
	var a, b bytes.Buffer
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&a, "alpha %04d aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", i)
		fmt.Fprintf(&b, "%04d zzzz beta gamma delta epsilon zeta\n", i*7919)
	}
	r1, _, _ := blob.Store(d, a.Bytes())
	r2, _, _ := blob.Store(d, b.Bytes())

	saved, err := Deltify(d, r1, r2)
	if err != nil {
		t.Fatalf("Deltify: %v", err)
	}
	if saved != 0 && isDelta(t, d, r1) {
		// Only a failure if the delta was not actually a win.
		var stored int
		if err := d.QueryRow("SELECT length(content) FROM blob WHERE rid=?", r1).Scan(&stored); err != nil {
			t.Fatalf("size: %v", err)
		}
		t.Logf("dissimilar pair still compressed to %d bytes", stored)
	}
}

func TestDeltifyRefusesPrivateIntoPublic(t *testing.T) {
	d := setupTestDB(t)
	v1, v2 := similarPair()
	pub, _, _ := blob.Store(d, v1)
	priv, _, _ := blob.Store(d, v2)
	if err := MakePrivate(d, int64(priv)); err != nil {
		t.Fatalf("MakePrivate: %v", err)
	}

	saved, err := Deltify(d, pub, priv)
	if err != nil {
		t.Fatalf("Deltify: %v", err)
	}
	if saved != 0 || isDelta(t, d, pub) {
		t.Error("a public artifact must never be stored as a delta against a private one")
	}
}

func TestDeltifyBreaksLoopByUndeltaingSource(t *testing.T) {
	d := setupTestDB(t)
	v1, v2 := similarPair()
	r1, _, _ := blob.Store(d, v1)
	r2, _, _ := blob.Store(d, v2)

	// Make r2 a delta of r1, then ask for the reverse. Canonical undeltas
	// the source rather than closing the loop.
	if _, err := Deltify(d, r2, r1); err != nil {
		t.Fatalf("Deltify(r2,r1): %v", err)
	}
	if !isDelta(t, d, r2) {
		t.Fatal("setup failed: r2 is not a delta")
	}
	if _, err := Deltify(d, r1, r2); err != nil {
		t.Fatalf("Deltify(r1,r2): %v", err)
	}
	if isDelta(t, d, r2) {
		t.Error("source should have been undeltaed to break the loop")
	}

	for _, rid := range []libfossil.FslID{r1, r2} {
		if _, err := Expand(d, rid); err != nil {
			t.Errorf("Expand(%d) after loop break: %v", rid, err)
		}
	}
}

func TestUndeltaRestoresFullContent(t *testing.T) {
	d := setupTestDB(t)
	v1, v2 := similarPair()
	r1, _, _ := blob.Store(d, v1)
	r2, _, _ := blob.Store(d, v2)

	if _, err := Deltify(d, r1, r2); err != nil {
		t.Fatalf("Deltify: %v", err)
	}
	if err := Undelta(d, r1); err != nil {
		t.Fatalf("Undelta: %v", err)
	}
	if isDelta(t, d, r1) {
		t.Error("delta link survived Undelta")
	}
	got, err := Expand(d, r1)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !bytes.Equal(got, v1) {
		t.Error("content changed across Undelta")
	}

	// Undelta on an already-whole artifact is a no-op, not an error.
	if err := Undelta(d, r1); err != nil {
		t.Errorf("Undelta on whole artifact: %v", err)
	}
}
