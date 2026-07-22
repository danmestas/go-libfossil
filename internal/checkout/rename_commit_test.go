package checkout

import (
	"bytes"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// newRenameTestCheckout returns a repo whose initial checkin is extracted into
// an in-memory checkout at /checkout, ready for rename/unmanage manipulation.
func newRenameTestCheckout(t *testing.T) (*repo.Repo, *Checkout, func()) {
	t.Helper()

	r, cleanupRepo := newTestRepoWithCheckin(t)

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		cleanupRepo()
		t.Fatal(err)
	}

	rid1, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}

	co.env = &simio.Env{Storage: simio.NewMemStorage(), Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	return r, co, func() {
		co.Close()
		cleanupRepo()
	}
}

// parseCommitManifest expands and parses the manifest for rid, returning both
// the parsed deck and the raw bytes for literal wire-format assertions.
func parseCommitManifest(t *testing.T, r *repo.Repo, rid libfossil.FslID) (*deck.Deck, []byte) {
	t.Helper()

	raw, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatalf("expand manifest: %v", err)
	}
	d, err := deck.Parse(raw)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return d, raw
}

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

// TestCommitRenameThenDeleteEmitsNoCard pins the rename-then-delete case:
// `fossil mv hello.txt greet.txt` followed by `fossil rm greet.txt` must
// commit no F-card at all for the file, under either name.
//
// Canonical Fossil excludes deleted rows from its manifest query outright
// (WHERE NOT deleted OR NOT is_selected). Here the deleted-files pass alone
// cannot achieve that: it only knows the file's current pathname, so retiring
// the old name from the parent manifest is applyRenames' job.
func TestCommitRenameThenDeleteEmitsNoCard(t *testing.T) {
	r, co, cleanup := newRenameTestCheckout(t)
	defer cleanup()

	if err := co.Rename(RenameOpts{From: "hello.txt", To: "greet.txt", DoFsMove: true}); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := co.Unmanage(UnmanageOpts{Paths: []string{"greet.txt"}}); err != nil {
		t.Fatalf("Unmanage: %v", err)
	}

	rid2, _, err := co.Commit(CommitOpts{Message: "rename then delete", User: "test"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	d, raw := parseCommitManifest(t, r, rid2)

	for _, f := range d.F {
		if f.Name == "hello.txt" || f.Name == "greet.txt" {
			t.Errorf("renamed-then-deleted file still emitted as %+v\nfull manifest:\n%s", f, raw)
		}
		if f.OldName == "hello.txt" {
			t.Errorf("card %+v carries the deleted file's prior name\nfull manifest:\n%s", f, raw)
		}
	}

	// The other two files from the initial checkin must survive untouched.
	if len(d.F) != 2 {
		t.Errorf("F-card count = %d, want 2 (src/main.go, README.md); cards = %+v", len(d.F), d.F)
	}
}

// TestCommitRenameChainEmitsBothCards pins a rename chain committed in one
// go: hello.txt → greet.txt while README.md → hello.txt reuses the freed name.
// Both files must be emitted once, each carrying its own prior name — the
// retired "hello.txt" entry must not be dropped, since a live vfile row has
// independently reclaimed that pathname.
func TestCommitRenameChainEmitsBothCards(t *testing.T) {
	r, co, cleanup := newRenameTestCheckout(t)
	defer cleanup()

	if err := co.Rename(RenameOpts{From: "hello.txt", To: "greet.txt", DoFsMove: true}); err != nil {
		t.Fatalf("Rename hello.txt: %v", err)
	}
	if err := co.Rename(RenameOpts{From: "README.md", To: "hello.txt", DoFsMove: true}); err != nil {
		t.Fatalf("Rename README.md: %v", err)
	}

	rid2, _, err := co.Commit(CommitOpts{Message: "rename chain", User: "test"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	d, raw := parseCommitManifest(t, r, rid2)

	want := map[string]string{
		"greet.txt": "hello.txt",
		"hello.txt": "README.md",
	}
	seen := make(map[string]int, len(want))
	for _, f := range d.F {
		if f.Name == "README.md" {
			t.Errorf("manifest still carries retired pathname README.md:\n%s", raw)
		}
		if oldName, ok := want[f.Name]; ok {
			seen[f.Name]++
			if f.OldName != oldName {
				t.Errorf("%s OldName = %q, want %q", f.Name, f.OldName, oldName)
			}
		}
	}
	for name := range want {
		if seen[name] != 1 {
			t.Errorf("%s emitted %d times, want exactly 1; cards = %+v", name, seen[name], d.F)
		}
	}

	// Content follows the file, not the name: greet.txt must keep hello.txt's
	// original bytes rather than picking up whatever now occupies hello.txt.
	for _, f := range d.F {
		if f.Name != "greet.txt" {
			continue
		}
		var rid int64
		if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid = ?", f.UUID).Scan(&rid); err != nil {
			t.Fatalf("resolve greet.txt blob: %v", err)
		}
		data, err := content.Expand(r.DB(), libfossil.FslID(rid))
		if err != nil {
			t.Fatalf("expand greet.txt: %v", err)
		}
		if !bytes.Equal(data, []byte("hello world\n")) {
			t.Errorf("greet.txt content = %q, want %q", data, "hello world\n")
		}
	}
}
