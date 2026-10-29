package libfossil

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// extSum is a made-up SHA-256: an external attachment's bytes are never here.
const extSum = "4f9a2c1d0b3e5f6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c"

// storeTestArtifact stores d as fossil would receive it and links it, for
// targets this library has no write API for (wiki pages, tech notes).
func storeTestArtifact(t *testing.T, r *Repo, d *deck.Deck) string {
	t.Helper()
	data, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	_, uuid, err := blob.Store(r.inner.DB(), data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(r.inner); err != nil {
		t.Fatal(err)
	}
	return uuid
}

func addWikiPage(t *testing.T, r *Repo, name string) {
	t.Helper()
	storeTestArtifact(t, r, &deck.Deck{
		Type: deck.Wiki, L: name, U: deck.User("alice"), W: []byte("page " + name + "\n"), D: ticketT0,
	})
}

// addTechNote makes a tech note with id and returns id.
func addTechNote(t *testing.T, r *Repo, id string) string {
	t.Helper()
	storeTestArtifact(t, r, &deck.Deck{
		Type: deck.Event, E: &deck.EventCard{Date: ticketT0, UUID: id},
		U: deck.User("alice"), D: ticketT0, W: []byte("note\n"), C: "A note",
	})
	return id
}

func mustAttach(t *testing.T, r *Repo, target, name, content string, opts AttachmentOpts) string {
	t.Helper()
	artifact, err := r.AttachmentAdd(target, name, []byte(content), opts)
	if err != nil {
		t.Fatalf("AttachmentAdd(%s, %s): %v", target, name, err)
	}
	return artifact
}

func artifactText(t *testing.T, r *Repo, hash string) string {
	t.Helper()
	data, err := r.ReadBlob(hash)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func blobCount(t *testing.T, r *Repo) int {
	t.Helper()
	var n int
	if err := r.inner.DB().QueryRow("SELECT count(*) FROM blob").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An attachment is exactly what fossil's attach_commit writes: the file as
// one ordinary blob, then an A C D U Z control artifact naming it, the
// directory dropped from the name and the comment trimmed.
func TestAttachmentAddWritesFossilsArtifacts(t *testing.T) {
	r := newTicketRepo(t)
	tkt, _ := mustTicketAdd(t, r, TicketOpts{Fields: map[string]string{"title": "x"}, User: "alice", Time: ticketT0})
	before := blobCount(t, r)
	when := ticketT0.Add(time.Minute)
	artifact := mustAttach(t, r, tkt[:10], "logs/crash log.txt", "boom\n",
		AttachmentOpts{User: "alice", Comment: "  the crash\n", Time: when})
	if got := blobCount(t, r) - before; got != 2 {
		t.Fatalf("attachment stored %d blobs, want 2 (the file and the control artifact)", got)
	}
	src := blobHashOf(t, r, "boom\n")
	want := fmt.Sprintf("A crash\\slog.txt %s %s\nC the\\scrash\nD %s\nU alice\n",
		tkt, src, when.UTC().Format("2006-01-02T15:04:05.000"))
	text := artifactText(t, r, artifact)
	if !strings.HasPrefix(text, want) || !strings.HasPrefix(text[len(want):], "Z ") {
		t.Fatalf("control artifact =\n%s\nwant prefix\n%s", text, want)
	}
	var unsent int
	if err := r.inner.DB().QueryRow(`SELECT count(*) FROM unsent WHERE rid IN
		(SELECT rid FROM blob WHERE uuid IN (?, ?))`, src, artifact).Scan(&unsent); err != nil || unsent != 2 {
		t.Fatalf("unsent holds %d of the two artifacts (%v)", unsent, err)
	}
	var kind, comment string
	if err := r.inner.DB().QueryRow(`SELECT type, comment FROM event WHERE objid=
		(SELECT rid FROM blob WHERE uuid=?)`, artifact).Scan(&kind, &comment); err != nil {
		t.Fatal(err)
	}
	if kind != "t" || !strings.HasPrefix(comment, "Add attachment [/artifact/") {
		t.Fatalf("timeline entry = %q %q", kind, comment)
	}
}

// blobHashOf is the hash the repository names content by.
func blobHashOf(t *testing.T, r *Repo, content string) string {
	t.Helper()
	var uuid string
	if err := r.inner.DB().QueryRow("SELECT uuid FROM blob WHERE size=? ORDER BY rid DESC LIMIT 1",
		len(content)).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	if got := artifactText(t, r, uuid); got != content {
		t.Fatalf("blob %s holds %q, want %q", uuid, got, content)
	}
	return uuid
}

// Each target kind lists and reads its own attachments, latest per name.
func TestAttachmentsPerTargetKind(t *testing.T) {
	r := newTicketRepo(t)
	tkt, _ := mustTicketAdd(t, r, TicketOpts{Fields: map[string]string{"title": "x"}, User: "alice", Time: ticketT0})
	addWikiPage(t, r, "Design Notes")
	note := addTechNote(t, r, "fedcba9876543210fedcba9876543210fedcba98")
	for i, target := range []string{tkt, "Design Notes", note} {
		at := ticketT0.Add(time.Duration(i+1) * time.Minute)
		mustAttach(t, r, target, "a.txt", "first "+target, AttachmentOpts{User: "alice", Time: at})
		mustAttach(t, r, target, "b.txt", "other", AttachmentOpts{User: "bob", Time: at.Add(time.Second)})
		mustAttach(t, r, target, "a.txt", "second "+target, AttachmentOpts{User: "carol", Time: at.Add(2 * time.Second)})
		list, err := r.Attachments(target)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 || list[0].Name != "a.txt" || list[1].Name != "b.txt" {
			t.Fatalf("%s: attachments = %+v", target, list)
		}
		a := list[0]
		if a.Target != target || a.User != "carol" || a.Size != int64(len("second "+target)) ||
			!sameMs(a.Time, at.Add(2*time.Second)) || a.External != nil {
			t.Fatalf("%s: latest a.txt = %+v", target, a)
		}
		got, content, err := r.AttachmentRead(target, "a.txt")
		if err != nil || string(content) != "second "+target || got != a {
			t.Fatalf("%s: AttachmentRead = %+v %q %v", target, got, content, err)
		}
	}
	// The ticket's and tech note's attachments resolve by prefix.
	if list, err := r.Attachments(tkt[:8]); err != nil || len(list) != 2 {
		t.Fatalf("Attachments(ticket prefix) = %v, %v", list, err)
	}
	if list, err := r.Attachments("fedcba98"); err != nil || len(list) != 2 {
		t.Fatalf("Attachments(tech note prefix) = %v, %v", list, err)
	}
}

// A deletion is an A card with no source and no C card; the name drops out
// of the list and the bytes stay in history.
func TestAttachmentDelete(t *testing.T) {
	r := newTicketRepo(t)
	addWikiPage(t, r, "Home")
	mustAttach(t, r, "Home", "x.txt", "x", AttachmentOpts{User: "alice", Comment: "c", Time: ticketT0})
	when := ticketT0.Add(time.Minute)
	artifact, err := r.AttachmentDelete("Home", "x.txt", AttachmentOpts{User: "bob", Time: when})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("A x.txt Home\nD %s\nU bob\nZ ", when.UTC().Format("2006-01-02T15:04:05.000"))
	if text := artifactText(t, r, artifact); !strings.HasPrefix(text, want) {
		t.Fatalf("deletion artifact =\n%s\nwant prefix\n%s", text, want)
	}
	if list, err := r.Attachments("Home"); err != nil || len(list) != 0 {
		t.Fatalf("Attachments after delete = %v, %v", list, err)
	}
	if _, _, err := r.AttachmentRead("Home", "x.txt"); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("AttachmentRead after delete: %v", err)
	}
	if _, err := r.AttachmentDelete("Home", "x.txt", AttachmentOpts{User: "bob"}); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	mustAttach(t, r, "Home", "x.txt", "back", AttachmentOpts{User: "alice", Time: when.Add(time.Minute)})
	if _, content, err := r.AttachmentRead("Home", "x.txt"); err != nil || string(content) != "back" {
		t.Fatalf("re-added = %q, %v", content, err)
	}
}

func TestAttachmentRejectsBadInput(t *testing.T) {
	r := newTicketRepo(t)
	addWikiPage(t, r, "Home")
	tkt, _ := mustTicketAdd(t, r, TicketOpts{Fields: map[string]string{"title": "x"}, User: "alice", Time: ticketT0})
	// A hex-looking page name no id starts with: its first digit is not the
	// ticket's, and the tech note below shares the ticket's.
	hexPage := "abc"
	if tkt[0] == 'a' {
		hexPage = "bcd"
	}
	addWikiPage(t, r, hexPage)
	addTechNote(t, r, tkt[:2]+"00000000000000000000000000000000000000")
	mustAttach(t, r, "Home", "x.txt", "x", AttachmentOpts{User: "alice", Time: ticketT0})
	opts := AttachmentOpts{User: "alice"}
	cases := map[string]error{}
	_, cases["no user"] = r.AttachmentAdd("Home", "y.txt", []byte("y"), AttachmentOpts{})
	_, cases["unknown target"] = r.AttachmentAdd("Nowhere", "y.txt", []byte("y"), opts)
	_, cases["ambiguous prefix"] = r.AttachmentAdd(tkt[:2], "y.txt", []byte("y"), opts)
	_, cases["bad name"] = r.AttachmentAdd("Home", "dir/..", []byte("y"), opts)
	_, cases["delete comment"] = r.AttachmentDelete("Home", "x.txt", AttachmentOpts{User: "alice", Comment: "why"})
	_, cases["delete no user"] = r.AttachmentDelete("Home", "x.txt", AttachmentOpts{})
	_, cases["delete too early"] = r.AttachmentDelete("Home", "x.txt", AttachmentOpts{User: "alice", Time: ticketT0})
	_, cases["delete unknown"] = r.AttachmentDelete("Home", "nope.txt", opts)
	_, cases["external dir"] = r.AttachmentAddExternal("Home",
		ExternalRef{SHA256: extSum, Size: 1, Type: "text/plain", Name: "d/x"}, opts)
	_, cases["external bad sha"] = r.AttachmentAddExternal("Home",
		ExternalRef{SHA256: extSum[:63], Size: 1, Type: "text/plain", Name: "x"}, opts)
	_, cases["external no type"] = r.AttachmentAddExternal("Home",
		ExternalRef{SHA256: extSum, Size: 1, Name: "x"}, opts)
	_, cases["external neg size"] = r.AttachmentAddExternal("Home",
		ExternalRef{SHA256: extSum, Size: -1, Type: "text/plain", Name: "x"}, opts)
	_, cases["external two lines"] = r.AttachmentAddExternal("Home",
		ExternalRef{SHA256: extSum, Size: 1, Type: "text/plain", Name: "x\nurl y"}, opts)
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if list, err := r.Attachments("Home"); err != nil || len(list) != 1 {
		t.Fatalf("a refused write left attachments %v, %v", list, err)
	}
	// An exact wiki name that is also hex wins only when no id shares it.
	if _, err := r.AttachmentAdd(hexPage, "y.txt", []byte("y"), opts); err != nil {
		t.Fatalf("hex-looking wiki name: %v", err)
	}
}

// A file that parses as an artifact is stored compressed under name.gz, as
// fossil stores it, so it is never linked as a control artifact.
func TestAttachmentOfArtifactIsCompressed(t *testing.T) {
	r := newTicketRepo(t)
	addWikiPage(t, r, "Home")
	art, err := (&deck.Deck{Type: deck.Wiki, L: "Other", U: deck.User("x"), W: []byte("w\n"), D: ticketT0}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	mustAttach(t, r, "Home", "page.txt", string(art), AttachmentOpts{User: "alice", Time: ticketT0})
	if _, _, err := r.AttachmentRead("Home", "page.txt"); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("artifact attached under its own name: %v", err)
	}
	_, packed, err := r.AttachmentRead("Home", "page.txt.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(packed[4:]))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(plain, art) {
		t.Fatalf("decompressed = %q, %v", plain, err)
	}
	var wikis int
	if err := r.inner.DB().QueryRow("SELECT count(*) FROM tag WHERE tagname='wiki-Other'").Scan(&wikis); err != nil || wikis != 0 {
		t.Fatalf("the attached artifact was linked as a wiki page (%d, %v)", wikis, err)
	}
}

// An external attachment stores Quarry's stub, carries the mimetype on its
// N card, and reads back as the reference rather than as the stub's text.
func TestAttachmentExternalRoundTrip(t *testing.T) {
	r := newTicketRepo(t)
	addWikiPage(t, r, "Home")
	ref := ExternalRef{SHA256: extSum, Size: 18349022, Type: "video/mp4", Name: "screen recording.mp4"}
	artifact, err := r.AttachmentAddExternal("Home", ref, AttachmentOpts{User: "alice", Comment: "repro", Time: ticketT0})
	if err != nil {
		t.Fatal(err)
	}
	stub := "quarry-attachment 1\nsha256 " + extSum + "\nsize 18349022\ntype video/mp4\nname screen recording.mp4\n"
	src := blobHashOf(t, r, stub)
	text := artifactText(t, r, artifact)
	want := fmt.Sprintf("A screen\\srecording.mp4 Home %s\nC repro\nD %s\nN video/mp4\nU alice\nZ ",
		src, ticketT0.UTC().Format("2006-01-02T15:04:05.000"))
	if !strings.HasPrefix(text, want) {
		t.Fatalf("control artifact =\n%s\nwant prefix\n%s", text, want)
	}
	list, err := r.Attachments("Home")
	if err != nil || len(list) != 1 {
		t.Fatalf("Attachments = %v, %v", list, err)
	}
	if a := list[0]; a.External == nil || *a.External != ref || a.Size != ref.Size || a.Source != src {
		t.Fatalf("listed = %+v", a)
	}
	a, content, err := r.AttachmentRead("Home", ref.Name)
	if err != nil || content != nil || a.External == nil || *a.External != ref {
		t.Fatalf("AttachmentRead = %+v %q %v", a, content, err)
	}
	// Adding the stub's text as an ordinary file would read back as this
	// reference, not as the bytes given, so it is refused.
	if _, err := r.AttachmentAdd("Home", "stub.txt", []byte(stub), AttachmentOpts{User: "alice"}); err == nil {
		t.Fatal("a well-formed stub was accepted as ordinary content")
	}
}

// A stub written by another tool -- Quarry's bytes and an A card naming it,
// as zit writes them -- is recognised; a blob that opens like a stub and is
// malformed is an ordinary file, as Quarry's index folds it.
func TestAttachmentStubRecognition(t *testing.T) {
	r := newTicketRepo(t)
	addWikiPage(t, r, "Home")
	quarry := "quarry-attachment 1\nsha256 " + extSum + "\nsize 12\ntype image/png\nname a.png\n"
	_, src, err := blob.Store(r.inner.DB(), []byte(quarry))
	if err != nil {
		t.Fatal(err)
	}
	storeTestArtifact(t, r, &deck.Deck{
		Type: deck.Attachment, A: &deck.AttachmentCard{Filename: "a.png", Target: "Home", Source: src},
		D: ticketT0, N: "image/png", U: deck.User("quarry"),
	})
	a, content, err := r.AttachmentRead("Home", "a.png")
	want := ExternalRef{SHA256: extSum, Size: 12, Type: "image/png", Name: "a.png"}
	if err != nil || content != nil || a.External == nil || *a.External != want {
		t.Fatalf("hand-written stub read as %+v %q %v", a, content, err)
	}
	malformed := "quarry-attachment 1\nsha256 " + extSum + "\nsize 12\ntype image/png\n"
	mustAttach(t, r, "Home", "notes.txt", malformed, AttachmentOpts{User: "alice", Time: ticketT0})
	a, content, err = r.AttachmentRead("Home", "notes.txt")
	if err != nil || string(content) != malformed || a.External != nil || a.Size != int64(len(malformed)) {
		t.Fatalf("malformed stub read as %+v %q %v", a, content, err)
	}
}

// Attachments against a stock fossil binary, in the ticket interop topology:
// this library's repository serves sync; fossil clones, pulls and pushes.
// Fossil 2.28 has no `attachment list` or `attachment delete` command, so
// lists come from `fossil test-list-attachments` and the web-only paths
// (ticket targets, deletion) run through `fossil test-http`.

// fossilWeb sends one request to fossil's web UI over the clone, as a
// browser would: a POST must carry a same-origin Referer or fossil opens the
// repository read-only.
func (x *ticketInterop) fossilWeb(t *testing.T, method, path, contentType string, body []byte) string {
	t.Helper()
	req := fmt.Sprintf("%s %s HTTP/1.0\r\nHost: localhost\r\nReferer: http://localhost%s\r\n"+
		"Origin: http://localhost\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
		method, path, path, contentType, len(body))
	// test-http writes fossil's global config like any command; it must land
	// in the home newTicketInterop's RequireFossilBin isolated.
	home := os.Getenv("FOSSIL_HOME")
	if home == "" {
		t.Fatal("fossilWeb runs only under the fossil home RequireFossilBin isolates")
	}
	cmd := exec.Command(x.bin, "test-http", x.clone)
	cmd.Dir = filepath.Dir(x.clone)
	cmd.Env = append(os.Environ(), "USER=bob", "FOSSIL_HOME="+home)
	cmd.Stdin = io.MultiReader(strings.NewReader(req), bytes.NewReader(body))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil test-http %s %s: %v\n%s", method, path, err, out)
	}
	if status, _, _ := strings.Cut(string(out), "\r\n"); !strings.Contains(status, " 200 ") &&
		!strings.Contains(status, " 302 ") {
		t.Fatalf("fossil test-http %s %s: %s", method, path, status)
	}
	return string(out)
}

// fossilWebAttach attaches content to ticket tkt through /attachadd, the
// only way stock fossil attaches to a ticket.
func (x *ticketInterop) fossilWebAttach(t *testing.T, tkt, name, content, comment string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, f := range [][2]string{{"tkt", tkt}, {"comment", comment}, {"ok", "Add Attachment"}} {
		if err := w.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	part, err := w.CreateFormFile("f", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	x.fossilWeb(t, "POST", "/attachadd", w.FormDataContentType(), body.Bytes())
}

// fossilWebDelete deletes the attachment control artifact names, as the
// Delete button on fossil's /ainfo page does.
func (x *ticketInterop) fossilWebDelete(t *testing.T, control string) {
	t.Helper()
	out := x.fossilWeb(t, "POST", "/ainfo/"+control, "application/x-www-form-urlencoded",
		[]byte("confirm=Confirm"))
	if !strings.Contains(out, "has been deleted") {
		t.Fatalf("fossil did not delete attachment %s", control)
	}
}

// fossilControl is the hash of the newest control artifact attaching name.
func (x *ticketInterop) fossilControl(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSpace(x.fossil(t, "sql",
		"SELECT uuid FROM blob WHERE rid=(SELECT attachid FROM attachment WHERE filename='"+name+
			"' ORDER BY mtime DESC LIMIT 1)"))
}

// Attachments written here -- to a ticket, a wiki page and a tech note, a
// regular file, an external stub and a deletion -- clone into stock fossil
// byte for byte, list there as fossil lists its own, and survive a fossil
// rebuild that relinks them from the artifacts alone.
func TestAttachmentsWrittenHereReadInFossil(t *testing.T) {
	x := newTicketInterop(t)
	r := x.repo
	tkt, _ := mustTicketAdd(t, r, TicketOpts{Fields: map[string]string{"title": "x"}, User: "alice", Time: ticketT0})
	addWikiPage(t, r, "Home")
	note := addTechNote(t, r, "fedcba9876543210fedcba9876543210fedcba98")
	opts := AttachmentOpts{User: "alice", Comment: "a file", Time: ticketT0.Add(time.Minute)}
	controls := map[string]string{}
	for _, target := range []string{tkt, "Home", note} {
		controls[target] = mustAttach(t, r, target, "a.txt", "attached to "+target+"\n", opts)
	}
	ref := ExternalRef{SHA256: extSum, Size: 18349022, Type: "video/mp4", Name: "screen.mp4"}
	external, err := r.AttachmentAddExternal("Home", ref, opts)
	if err != nil {
		t.Fatal(err)
	}
	mustAttach(t, r, tkt, "gone.txt", "soon gone", opts)
	deletion, err := r.AttachmentDelete(tkt, "gone.txt", AttachmentOpts{User: "bob", Time: ticketT0.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	ticketFossil(t, x.bin, filepath.Dir(x.clone), "clone", x.url, x.clone)

	for round := 0; round < 2; round++ {
		for target, kind := range map[string]string{tkt: "t", "Home": "w", note: "e"} {
			a, content, err := r.AttachmentRead(target, "a.txt")
			if err != nil {
				t.Fatal(err)
			}
			requireLines(t, "fossil test-list-attachments", x.fossil(t, "test-list-attachments", "-latest", target),
				a.Source[:12]+" a.txt")
			if got := x.fossil(t, "artifact", a.Source); got != string(content) {
				t.Fatalf("fossil artifact %s = %q, want %q", a.Source, got, content)
			}
			if got := x.fossil(t, "artifact", controls[target]); got != artifactText(t, r, controls[target]) {
				t.Fatalf("fossil holds control artifact %s as %q", controls[target], got)
			}
			if got := strings.TrimSpace(x.fossil(t, "sql", "SELECT type FROM event WHERE objid="+
				"(SELECT rid FROM blob WHERE uuid='"+controls[target]+"')")); got != kind {
				t.Fatalf("fossil's timeline kind for %s = %s, want %s", target, got, kind)
			}
		}
		ext, _, err := r.AttachmentRead("Home", "screen.mp4")
		if err != nil {
			t.Fatal(err)
		}
		requireLines(t, "fossil test-list-attachments", x.fossil(t, "test-list-attachments", "-latest", "Home"),
			ext.Source[:12]+" screen.mp4")
		stub := x.fossil(t, "artifact", ext.Source)
		if parsed := strings.Split(stub, "\n"); len(parsed) != 6 || parsed[0] != "quarry-attachment 1" ||
			parsed[3] != "type video/mp4" || parsed[4] != "name screen.mp4" {
			t.Fatalf("fossil shows the stub as %q", stub)
		}
		requireLines(t, "fossil artifact", x.fossil(t, "artifact", external), "N video/mp4\n")
		requireLines(t, "fossil test-list-attachments", x.fossil(t, "test-list-attachments", "-latest", tkt),
			"(null) gone.txt")
		requireLines(t, "fossil artifact", x.fossil(t, "artifact", deletion), "A gone.txt "+tkt+"\nD ")
		// Fossil relinks every artifact from its bytes alone; what it
		// derives must be what it derived on clone.
		x.fossil(t, "rebuild")
	}
}

// Attachments made by stock fossil -- the CLI for wiki pages and tech notes,
// the web UI for tickets and deletion -- push here and list and read as
// fossil shows them. A hand-written Quarry stub is recognised; a malformed
// one is a file. A deletion made here pulls back into fossil.
func TestFossilAttachmentsReadHere(t *testing.T) {
	x := newTicketInterop(t)
	dir := filepath.Dir(x.clone)
	ticketFossil(t, x.bin, dir, "clone", x.url, x.clone)
	x.fossil(t, "user", "default", "bob")
	stub := "quarry-attachment 1\nsha256 " + extSum + "\nsize 12\ntype image/png\nname shot.png\n"
	files := map[string]string{
		"body.txt": "page\n", "a.txt": "from fossil\n", "gone.txt": "gone\n",
		"shot.png": stub, "half.txt": strings.TrimSuffix(stub, "name shot.png\n"),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	x.fossil(t, "wiki", "create", "Home", "body.txt")
	x.fossil(t, "wiki", "create", "Note", "body.txt", "--technote", "2026-01-02T03:04:05")
	note := strings.Fields(x.fossil(t, "wiki", "ls", "--technote", "--show-technote-ids"))[0]
	added := strings.Fields(x.fossil(t, "ticket", "add", "title", "T"))
	tkt := added[len(added)-1]
	for _, name := range []string{"a.txt", "shot.png", "half.txt", "gone.txt"} {
		x.fossil(t, "attachment", "add", "Home", name)
	}
	x.fossil(t, "attachment", "add", "a.txt", "--technote", note)
	x.fossilWebAttach(t, tkt, "a.txt", files["a.txt"], "from the web")
	x.fossilWebDelete(t, x.fossilControl(t, "gone.txt"))
	ticketFossil(t, x.bin, dir, "push", x.url, "-R", x.clone)

	list, err := x.repo.Attachments("Home")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]Attachment{}
	for _, a := range list {
		names[a.Name] = a
	}
	if len(list) != 3 || names["a.txt"].User != "bob" || names["half.txt"].External != nil {
		t.Fatalf("Home's attachments here = %+v", list)
	}
	want := ExternalRef{SHA256: extSum, Size: 12, Type: "image/png", Name: "shot.png"}
	if e := names["shot.png"].External; e == nil || *e != want {
		t.Fatalf("fossil-attached stub reads as %+v", names["shot.png"])
	}
	for target, name := range map[string]string{"Home": "half.txt", note: "a.txt", tkt: "a.txt"} {
		_, content, err := x.repo.AttachmentRead(target, name)
		if err != nil || string(content) != files[name] {
			t.Fatalf("%s %s reads here as %q, %v", target, name, content, err)
		}
	}
	if a, _, err := x.repo.AttachmentRead(tkt[:10], "a.txt"); err != nil || a.Comment != "from the web" {
		t.Fatalf("ticket attachment = %+v, %v", a, err)
	}
	if _, err := x.repo.AttachmentDelete("Home", "a.txt", AttachmentOpts{User: "editor"}); err != nil {
		t.Fatal(err)
	}
	ticketFossil(t, x.bin, dir, "pull", x.url, "-R", x.clone)
	requireLines(t, "fossil test-list-attachments", x.fossil(t, "test-list-attachments", "-latest", "Home"),
		"(null) a.txt", "(null) gone.txt")
}

// requireCurrent checks the list and the read agree on whether name is
// current on target, and that its bytes are want when it is.
func requireCurrent(t *testing.T, r *Repo, target, name, want string, current bool) {
	t.Helper()
	list, err := r.Attachments(target)
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, a := range list {
		if a.Name == name {
			listed++
		}
	}
	_, content, err := r.AttachmentRead(target, name)
	switch {
	case current && (listed != 1 || err != nil || string(content) != want):
		t.Fatalf("%s should be current: listed %d times, read %q, %v", name, listed, content, err)
	case !current && (listed != 0 || !errors.Is(err, ErrAttachmentNotFound)):
		t.Fatalf("%s should be gone: listed %d times, read error %v", name, listed, err)
	}
}

// Adds and deletes at the default time, faster than the D card's
// millisecond, each become the latest for the name: a tie would leave two
// rows latest, listing a deleted file that cannot be read.
func TestAttachmentDefaultTimesNeverTie(t *testing.T) {
	r := newTicketRepo(t)
	addWikiPage(t, r, "Home")
	opts := AttachmentOpts{User: "alice"}
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("round %d", i)
		mustAttach(t, r, "Home", "x.txt", content, opts)
		requireCurrent(t, r, "Home", "x.txt", content, true)
		if _, err := r.AttachmentDelete("Home", "x.txt", opts); err != nil {
			t.Fatal(err)
		}
		requireCurrent(t, r, "Home", "x.txt", "", false)
	}
}

// An attachment stamped in the future -- a synced one from a fast clock --
// is still deleted and replaced by default-time writes, which land after
// it; an explicit time no later than the latest is refused.
func TestAttachmentAfterFutureAttachment(t *testing.T) {
	r := newTicketRepo(t)
	addWikiPage(t, r, "Home")
	future := time.Now().Add(time.Hour)
	mustAttach(t, r, "Home", "x.txt", "future", AttachmentOpts{User: "alice", Time: future})
	if _, err := r.AttachmentDelete("Home", "x.txt", AttachmentOpts{User: "bob", Time: future}); err == nil {
		t.Fatal("a deletion at the attachment's own time was accepted")
	}
	if _, err := r.AttachmentAdd("Home", "x.txt", []byte("older"), AttachmentOpts{User: "bob", Time: future}); err == nil {
		t.Fatal("a re-add at the latest's own time was accepted")
	}
	if _, err := r.AttachmentDelete("Home", "x.txt", AttachmentOpts{User: "bob"}); err != nil {
		t.Fatal(err)
	}
	requireCurrent(t, r, "Home", "x.txt", "", false)
	mustAttach(t, r, "Home", "x.txt", "again", AttachmentOpts{User: "bob"})
	requireCurrent(t, r, "Home", "x.txt", "again", true)
	if a, _, err := r.AttachmentRead("Home", "x.txt"); err != nil || !a.Time.After(future) {
		t.Fatalf("re-add stamped %v, not after %v (%v)", a.Time, future, err)
	}
}

func TestAttachmentRefusalsAreSpecific(t *testing.T) {
	r := newTicketRepo(t)
	addWikiPage(t, r, "Home")
	tkt, _ := mustTicketAdd(t, r, TicketOpts{Fields: map[string]string{"title": "x"}, User: "alice", Time: ticketT0})
	addTechNote(t, r, tkt[:2]+"00000000000000000000000000000000000000")
	_, err := r.AttachmentAdd(tkt[:2], "y.txt", []byte("y"), AttachmentOpts{User: "alice"})
	if err == nil || !strings.Contains(err.Error(), "ticket") || !strings.Contains(err.Error(), "tech note") {
		t.Fatalf("ambiguity error does not name the kinds: %v", err)
	}
	for _, name := range []string{".", ".."} {
		_, err := r.AttachmentAddExternal("Home",
			ExternalRef{SHA256: extSum, Size: 1, Type: "text/plain", Name: name}, AttachmentOpts{User: "alice"})
		if err == nil || !strings.Contains(err.Error(), "external name") {
			t.Fatalf("external name %q: %v", name, err)
		}
	}
}
