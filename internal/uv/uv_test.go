package uv

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// openTestDB creates a temporary repo DB with schema for testing.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.CreateRepoSchema(d); err != nil {
		d.Close()
		t.Fatalf("CreateRepoSchema: %v", err)
	}
	if err := EnsureSchema(d); err != nil {
		d.Close()
		t.Fatalf("EnsureSchema: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestEnsureSchema(t *testing.T) {
	d := openTestDB(t)
	// Calling twice should be idempotent.
	if err := EnsureSchema(d); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}
}

func TestWriteAndRead(t *testing.T) {
	d := openTestDB(t)

	if err := Write(d, "test.txt", []byte("hello world"), 1700000000); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, mtime, hash, err := Read(d, "test.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(content) != "hello world" {
		t.Errorf("content = %q, want %q", content, "hello world")
	}
	if mtime != 1700000000 {
		t.Errorf("mtime = %d, want %d", mtime, 1700000000)
	}
	if hash == "" {
		t.Error("hash should not be empty")
	}
}

func TestWriteOverwrite(t *testing.T) {
	d := openTestDB(t)
	Write(d, "f.txt", []byte("v1"), 100)
	Write(d, "f.txt", []byte("v2"), 200)

	content, mtime, _, err := Read(d, "f.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(content) != "v2" || mtime != 200 {
		t.Errorf("got content=%q mtime=%d, want v2/200", content, mtime)
	}
}

func TestDeleteAndRead(t *testing.T) {
	d := openTestDB(t)
	Write(d, "test.txt", []byte("hello"), 100)
	if err := Delete(d, "test.txt", 200); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	content, mtime, hash, err := Read(d, "test.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content != nil {
		t.Errorf("content should be nil for tombstone, got %q", content)
	}
	if mtime != 200 {
		t.Errorf("mtime = %d, want 200", mtime)
	}
	if hash != "" {
		t.Errorf("hash should be empty for tombstone, got %q", hash)
	}
}

func TestReadNonExistent(t *testing.T) {
	d := openTestDB(t)
	content, mtime, hash, err := Read(d, "nope.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content != nil || mtime != 0 || hash != "" {
		t.Errorf("expected zero values for non-existent file")
	}
}

func TestList(t *testing.T) {
	d := openTestDB(t)
	Write(d, "a.txt", []byte("aaa"), 100)
	Write(d, "b.txt", []byte("bbb"), 200)
	Delete(d, "c.txt", 300)

	entries, err := List(d)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
}

func TestContentHashEmpty(t *testing.T) {
	d := openTestDB(t)
	h, err := ContentHash(d)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	// SHA1 of empty string
	if h != "da39a3ee5e6b4b0d3255bfef95601890afd80709" {
		t.Errorf("empty hash = %q, want da39a3ee...", h)
	}
}

func TestContentHashDeterministic(t *testing.T) {
	d := openTestDB(t)
	Write(d, "b.txt", []byte("bbb"), 200)
	Write(d, "a.txt", []byte("aaa"), 100)

	h1, _ := ContentHash(d)
	h2, _ := ContentHash(d)
	if h1 != h2 {
		t.Errorf("ContentHash not deterministic: %q != %q", h1, h2)
	}
}

func TestContentHashExcludesTombstones(t *testing.T) {
	d := openTestDB(t)
	Write(d, "a.txt", []byte("aaa"), 100)

	h1, _ := ContentHash(d)

	Delete(d, "b.txt", 200) // tombstone should not affect hash

	h2, _ := ContentHash(d)
	if h1 != h2 {
		t.Errorf("tombstone changed hash: %q != %q", h1, h2)
	}
}

func TestContentHashChangesOnWrite(t *testing.T) {
	d := openTestDB(t)
	Write(d, "a.txt", []byte("v1"), 100)
	h1, _ := ContentHash(d)

	Write(d, "a.txt", []byte("v2"), 200)
	h2, _ := ContentHash(d)

	if h1 == h2 {
		t.Error("hash should change after write")
	}
}

func TestInvalidateHash(t *testing.T) {
	d := openTestDB(t)
	Write(d, "a.txt", []byte("aaa"), 100)
	h1, _ := ContentHash(d) // caches

	// Manually update without going through Write (simulating external change).
	d.Exec("UPDATE unversioned SET mtime=999 WHERE name='a.txt'")
	InvalidateHash(d)

	h2, _ := ContentHash(d)
	if h1 == h2 {
		t.Error("hash should differ after invalidate + mtime change")
	}
}

// TestReceive pins §7.3's handling of a received uvfile, as fossil's
// xfer_accept_unversioned_file applies it: the row keeps the HASH the card
// gave, the same content at a later mtime moves only the mtime, and omitted
// content is stored as NULL with size 0.
func TestReceive(t *testing.T) {
	d := openTestDB(t)
	sha1Name := "2aae6c35c94fcfb415dbe95f408b9ce91ee846ed" // SHA1("hello world")

	if err := Receive(d, "a.txt", 100, sha1Name, []byte("hello world")); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	content, mtime, got, err := Read(d, "a.txt")
	if err != nil || got != sha1Name || mtime != 100 || string(content) != "hello world" {
		t.Fatalf("a.txt = %q, %d, %q, %v; want the card's HASH", content, mtime, got, err)
	}

	if err := Receive(d, "a.txt", 200, sha1Name, nil); err != nil {
		t.Fatalf("Receive status 2: %v", err)
	}
	content, mtime, _, _ = Read(d, "a.txt")
	if mtime != 200 || string(content) != "hello world" {
		t.Fatalf("after status 2: mtime %d, content %q; want 200 and the content kept",
			mtime, content)
	}

	if err := Receive(d, "w.txt", 100, sha1Name, nil); err != nil {
		t.Fatalf("Receive omitted: %v", err)
	}
	var sz int
	var isNull bool
	err = d.QueryRow("SELECT sz, content IS NULL FROM unversioned WHERE name='w.txt'").
		Scan(&sz, &isNull)
	if err != nil || sz != 0 || !isNull {
		t.Fatalf("omitted content: sz %d, NULL %v, %v; want 0 and NULL", sz, isNull, err)
	}
}

// TestReceiveDeletionOfAbsentName pins that a received deletion for a name
// the repository does not hold changes nothing, as fossil's UPDATE does,
// while a local Delete still records a tombstone.
func TestReceiveDeletionOfAbsentName(t *testing.T) {
	d := openTestDB(t)
	if err := Receive(d, "never.txt", 100, "-", nil); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	var n int
	if err := d.QueryRow("SELECT count(*) FROM unversioned").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after a received deletion of an absent name: %d, %v; want 0", n, err)
	}
	if err := Delete(d, "never.txt", 100); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := d.QueryRow("SELECT count(*) FROM unversioned").Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after a local Delete: %d, %v; want 1", n, err)
	}
}

// TestContentHashFossilizesNames checks sync-protocol-01 §8.5.1: the catalog
// hashes each name in its fossilized form and sorts by those bytes. The
// unfossilized order puts "a b.txt" first; the fossilized one, "a\sb.txt",
// puts it after "a-b.txt".
func TestContentHashFossilizesNames(t *testing.T) {
	d := openTestDB(t)
	if err := Write(d, "a b.txt", []byte("one"), 100); err != nil {
		t.Fatal(err)
	}
	if err := Write(d, "a-b.txt", []byte("two"), 200); err != nil {
		t.Fatal(err)
	}
	var h1, h2 string
	var d1, d2 string
	if err := d.QueryRow("SELECT hash, datetime(mtime,'unixepoch') FROM unversioned"+
		" WHERE name='a b.txt'").Scan(&h1, &d1); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow("SELECT hash, datetime(mtime,'unixepoch') FROM unversioned"+
		" WHERE name='a-b.txt'").Scan(&h2, &d2); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum([]byte("a-b.txt " + d2 + " " + h2 + "\n" +
		`a\sb.txt ` + d1 + " " + h1 + "\n"))
	want := hex.EncodeToString(sum[:])

	got, err := ContentHash(d)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ContentHash = %s, want %s", got, want)
	}
}
