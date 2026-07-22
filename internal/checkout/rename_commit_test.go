package checkout

import (
	"bytes"
	"testing"

	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/simio"
)

// TestCommitRenameEmitsSingleRenameCard pins the commit-time file-list
// assembly and F-card serialization for a pure rename (no content change),
// covering both defects from #51.
//
// Methodology: extract the initial checkin into MemStorage, rename hello.txt
// to greet.txt (moving the file on disk too), commit, then re-expand and parse
// the generated manifest. Assert three properties of its F-cards:
//
//   - The renamed file appears exactly once, under its NEW name (greet.txt),
//     with the prior name recorded as OldName — no stale hello.txt card
//     survives from the parent manifest (the double-emit defect).
//   - No F-card carries the old pathname (hello.txt) as its own Name.
//   - The serialized card forces canonical Fossil's " w" permission
//     placeholder (checkin.c ~1999) so the prior-name field keeps its 4th
//     positional slot: "F greet.txt <uuid> w hello.txt".
//
// The literal-byte assertion mirrors stock fossil's output, verified against a
// real fossil binary during development.
func TestCommitRenameEmitsSingleRenameCard(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid1, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}

	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Pure rename: move the file on disk, retire the old vfile pathname.
	if err := co.Rename(RenameOpts{From: "hello.txt", To: "greet.txt", DoFsMove: true}); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	rid2, _, err := co.Commit(CommitOpts{Message: "rename hello to greet", User: "test"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	raw, err := content.Expand(r.DB(), rid2)
	if err != nil {
		t.Fatalf("expand manifest: %v", err)
	}
	d, err := deck.Parse(raw)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	var greet *deck.FileCard
	for i := range d.F {
		if d.F[i].Name == "hello.txt" {
			t.Errorf("manifest still carries old pathname hello.txt as its own F-card: %+v", d.F[i])
		}
		if d.F[i].Name == "greet.txt" {
			if greet != nil {
				t.Fatalf("greet.txt emitted more than once")
			}
			greet = &d.F[i]
		}
	}
	if greet == nil {
		t.Fatalf("no F-card for renamed file greet.txt; cards = %+v", d.F)
	}
	if greet.OldName != "hello.txt" {
		t.Errorf("greet.txt OldName = %q, want %q", greet.OldName, "hello.txt")
	}

	// Literal wire-format assertion: the serialized card must force the " w"
	// placeholder and keep the prior name in the 4th field.
	line := []byte("F greet.txt " + greet.UUID + " w hello.txt\n")
	if !bytes.Contains(raw, line) {
		t.Errorf("manifest missing canonical rename card %q\nfull manifest:\n%s", line, raw)
	}
	if bytes.Contains(raw, []byte("\nF hello.txt")) || bytes.HasPrefix(raw, []byte("F hello.txt")) {
		t.Errorf("manifest double-emits old pathname hello.txt:\n%s", raw)
	}
}
