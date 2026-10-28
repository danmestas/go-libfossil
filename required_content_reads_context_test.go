package libfossil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/hash"
	internalsync "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// gimmeRecorder is a peer that records each exchange and every artifact a
// request asks for by gimme.
type gimmeRecorder struct {
	t         *testing.T
	peer      Transport
	exchanges int
	gimmes    []string
}

func (g *gimmeRecorder) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	reply, _, err := g.RoundTripFramed(ctx, payload)
	return reply, err
}

func (g *gimmeRecorder) RoundTripFramed(ctx context.Context, payload []byte) ([]byte, string, error) {
	g.t.Helper()
	message, err := xfer.Decode(payload, xfer.ContentTypeCompressed)
	if err != nil {
		g.t.Fatal(err)
	}
	g.exchanges++
	for _, card := range message.Cards {
		if gimme, ok := card.(*xfer.GimmeCard); ok {
			g.gimmes = append(g.gimmes, gimme.UUID)
		}
	}
	if framed, ok := g.peer.(FramedTransport); ok {
		return framed.RoundTripFramed(ctx, payload)
	}
	reply, err := g.peer.RoundTrip(ctx, payload)
	return reply, "", err
}

// sorted returns the gimmes asked for, in order, for comparison.
func (g *gimmeRecorder) sorted() []string {
	out := slices.Clone(g.gimmes)
	slices.Sort(out)
	return out
}

func sortedNames(names ...string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}

func horizonClone(t *testing.T, peer Transport, c string, structural bool) *Repo {
	t.Helper()
	hc, err := NewHorizonCone(nil, nil, Horizon{Checkin: c, Structural: structural})
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "t.fossil"), peer,
		CloneOpts{StoreCone: hc, PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Close() })
	return target
}

func storeConeString(t *testing.T, r *Repo) string {
	t.Helper()
	c, err := r.StoreCone()
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("store cone is dense")
	}
	return c.String()
}

// A content-horizon clone reads a file at a check-in below its horizon C by
// fetching exactly that file's payload (draft-fossil-horizon-00 §4.4): the
// store cone and C do not move. Without a peer the read refuses naming C.
func TestReadFileContextContentHorizon(t *testing.T) {
	f := newHorizonFixture(t)
	target := horizonClone(t, f.peer, f.c2, false)
	before := storeConeString(t, target)
	rid, err := target.ResolveVersion(f.c1)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := target.ReadFile(rid, "a"); !errors.Is(err, ErrBelowHorizon) || !strings.Contains(err.Error(), f.c2) {
		t.Fatalf("ReadFile below the horizon: %v, want ErrBelowHorizon naming %s", err, f.c2)
	}
	_, err = target.ReadFileContext(context.Background(), rid, "a", RequiredContentOpts{})
	if !errors.Is(err, ErrBelowHorizon) || !strings.Contains(err.Error(), f.c2) {
		t.Fatalf("ReadFileContext without a peer: %v, want ErrBelowHorizon naming %s", err, f.c2)
	}

	peer := &gimmeRecorder{t: t, peer: f.peer}
	got, err := target.ReadFileContext(context.Background(), rid, "a", RequiredContentOpts{Peer: peer})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old a\n" {
		t.Fatalf("ReadFileContext = %q, want %q", got, "old a\n")
	}
	if want := []string{hash.SHA1([]byte("old a\n"))}; !slices.Equal(peer.sorted(), want) {
		t.Fatalf("gimmes = %v, want exactly the payload %v", peer.gimmes, want)
	}
	if after := storeConeString(t, target); after != before {
		t.Fatalf("store cone moved: %q, was %q", after, before)
	}
	// The fetched payload is held: the plain read now serves it.
	if got, err := target.ReadFile(rid, "a"); err != nil || string(got) != "old a\n" {
		t.Fatalf("ReadFile after the demand fetch = %q, %v", got, err)
	}
	// A read that needs nothing exchanges nothing.
	peer = &gimmeRecorder{t: t, peer: f.peer}
	if _, err := target.ReadFileAtContext(context.Background(), f.c2, "a", RequiredContentOpts{Peer: peer}); err != nil {
		t.Fatal(err)
	}
	if peer.exchanges != 0 {
		t.Fatalf("a held read exchanged %d time(s)", peer.exchanges)
	}
}

// A structural clone holds no manifest below C, so the read fetches the
// check-in's manifest (with its baseline and nodes, none here), then the
// payload the manifest names, and nothing else.
func TestReadFileContextStructuralHorizon(t *testing.T) {
	f := newHorizonFixture(t)
	target := horizonClone(t, f.peer, f.c2, true)
	before := storeConeString(t, target)

	_, err := target.ReadFileAt(f.c1, "a")
	if !errors.Is(err, ErrBelowHorizon) || !strings.Contains(err.Error(), f.c2) {
		t.Fatalf("ReadFileAt below the structural horizon: %v, want ErrBelowHorizon naming %s", err, f.c2)
	}
	peer := &gimmeRecorder{t: t, peer: f.peer}
	got, err := target.ReadFileAtContext(context.Background(), f.c1, "a", RequiredContentOpts{Peer: peer})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old a\n" {
		t.Fatalf("ReadFileAtContext = %q, want %q", got, "old a\n")
	}
	if want := sortedNames(f.c1, hash.SHA1([]byte("old a\n"))); !slices.Equal(peer.sorted(), want) {
		t.Fatalf("gimmes = %v, want the manifest and the payload %v", peer.gimmes, want)
	}
	if after := storeConeString(t, target); after != before {
		t.Fatalf("store cone moved: %q, was %q", after, before)
	}
}

// Diff fetches both sides' missing payloads in one demand fetch.
func TestDiffContextBelowHorizon(t *testing.T) {
	f := newHorizonFixture(t)
	target := horizonClone(t, f.peer, f.c2, false)
	rid1, err := target.ResolveVersion(f.c1)
	if err != nil {
		t.Fatal(err)
	}
	rid2, err := target.ResolveVersion(f.c2)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a", ""} {
		if _, err := target.Diff(rid1, rid2, path); !errors.Is(err, ErrBelowHorizon) {
			t.Fatalf("Diff %q below the horizon: %v, want ErrBelowHorizon", path, err)
		}
	}
	peer := &gimmeRecorder{t: t, peer: f.peer}
	entries, err := target.DiffContext(context.Background(), rid1, rid2, "", RequiredContentOpts{Peer: peer})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "a" || !strings.Contains(entries[0].Unified, "-old a") {
		t.Fatalf("DiffContext = %+v", entries)
	}
	if want := []string{hash.SHA1([]byte("old a\n"))}; !slices.Equal(peer.sorted(), want) || peer.exchanges != 1 {
		t.Fatalf("gimmes = %v over %d exchange(s), want %v in one", peer.gimmes, peer.exchanges, want)
	}
}

// linearHistory commits each of contents as file "a" on one line of
// descent, and returns a peer serving it and the check-ins, oldest first.
func linearHistory(t *testing.T, contents ...string) (Transport, []string) {
	t.Helper()
	source, err := Create(filepath.Join(t.TempDir(), "source.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	if err := source.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	var uuids []string
	var parent int64
	for _, text := range contents {
		rid, uuid, err := source.Commit(CommitOpts{User: "test", ParentID: parent,
			Files: []FileToCommit{{Name: "a", Content: []byte(text)}}})
		if err != nil {
			t.Fatal(err)
		}
		parent = rid
		uuids = append(uuids, uuid)
	}
	return TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return source.HandleSync(ctx, payload)
	}), uuids
}

type wantLine struct {
	uuid  string
	below bool
}

func checkAnnotation(t *testing.T, lines []AnnotatedLine, want []wantLine) {
	t.Helper()
	if len(lines) != len(want) {
		t.Fatalf("annotate returned %d lines, want %d", len(lines), len(want))
	}
	for i, w := range want {
		if lines[i].UUID != w.uuid || lines[i].BelowHorizon != w.below {
			t.Fatalf("line %d %q attributed to %.10s (below %v), want %.10s (below %v)",
				i, lines[i].Text, lines[i].UUID, lines[i].BelowHorizon, w.uuid, w.below)
		}
	}
}

// Without a peer, annotate on a content-horizon store reports the lines it
// cannot attribute past C as originating at the horizon, as a structural
// store does. With one, it fetches each version its walk reads below C and
// attributes every line to the check-in that added it.
func TestAnnotateContextBelowHorizon(t *testing.T) {
	peer, uuids := linearHistory(t, "l1\n", "l1\nl2\n", "l1\nl2\nl3\n", "l1\nl2\nl3\nl4\n")
	target := horizonClone(t, peer, uuids[2], false)
	tip, err := target.ResolveVersion(uuids[3])
	if err != nil {
		t.Fatal(err)
	}
	opts := AnnotateOpts{FilePath: "a", StartRID: tip}
	lines, err := target.Annotate(opts)
	if err != nil {
		t.Fatalf("Annotate across a content horizon: %v", err)
	}
	c := uuids[2]
	checkAnnotation(t, lines, []wantLine{{c, true}, {c, true}, {c, true}, {uuids[3], false}})

	rec := &gimmeRecorder{t: t, peer: peer}
	lines, err = target.AnnotateContext(context.Background(), opts, RequiredContentOpts{Peer: rec})
	if err != nil {
		t.Fatal(err)
	}
	want := sortedNames(hash.SHA1([]byte("l1\n")), hash.SHA1([]byte("l1\nl2\n")))
	if !slices.Equal(rec.sorted(), want) {
		t.Fatalf("gimmes = %v, want %v", rec.gimmes, want)
	}
	checkAnnotation(t, lines, []wantLine{{uuids[0], false}, {uuids[1], false}, {uuids[2], false}, {uuids[3], false}})
}

// Annotate fetches only the versions its walk reads: once every line is
// attributed the walk stops, and the older versions are not fetched.
func TestAnnotateContextFetchesOnlyWalkedVersions(t *testing.T) {
	peer, uuids := linearHistory(t, "a\n", "b\n", "c\n", "c\nd\n")
	target := horizonClone(t, peer, uuids[3], false)
	tip, err := target.ResolveVersion(uuids[3])
	if err != nil {
		t.Fatal(err)
	}
	rec := &gimmeRecorder{t: t, peer: peer}
	lines, err := target.AnnotateContext(context.Background(), AnnotateOpts{FilePath: "a", StartRID: tip},
		RequiredContentOpts{Peer: rec})
	if err != nil {
		t.Fatal(err)
	}
	if want := sortedNames(hash.SHA1([]byte("b\n")), hash.SHA1([]byte("c\n"))); !slices.Equal(rec.sorted(), want) {
		t.Fatalf("gimmes = %v, want only the walked versions %v", rec.gimmes, want)
	}
	checkAnnotation(t, lines, []wantLine{{uuids[2], false}, {uuids[3], false}})
}

// A walk needing more versions than one demand fetch may carry returns what
// it attributed, the rest marked at the horizon, and no error.
func TestAnnotateContextOverLimitIsPartial(t *testing.T) {
	peer, uuids := linearHistory(t, "l1\n", "l1\nl2\n", "l1\nl2\nl3\n", "l1\nl2\nl3\nl4\n")
	target := horizonClone(t, peer, uuids[2], false)
	tip, err := target.ResolveVersion(uuids[3])
	if err != nil {
		t.Fatal(err)
	}
	rec := &gimmeRecorder{t: t, peer: peer}
	lines, err := target.annotate(context.Background(), AnnotateOpts{FilePath: "a", StartRID: tip},
		RequiredContentOpts{Peer: rec}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{hash.SHA1([]byte("l1\nl2\n"))}; !slices.Equal(rec.sorted(), want) {
		t.Fatalf("gimmes = %v, want %v", rec.gimmes, want)
	}
	c := uuids[2]
	checkAnnotation(t, lines, []wantLine{{c, true}, {c, true}, {uuids[2], false}, {uuids[3], false}})
}

// An unheld payload that is not below the horizon (above C, outside the
// store cone's paths) is not labelled below the horizon and is not fetched.
func TestReadFileContextOutsidePathsNotBelowHorizon(t *testing.T) {
	source, err := Create(filepath.Join(t.TempDir(), "source.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	r1, c1, err := source.Commit(CommitOpts{User: "test", Files: []FileToCommit{
		{Name: "a", Content: []byte("a1\n")}, {Name: "b", Content: []byte("b1\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	_, c2, err := source.Commit(CommitOpts{User: "test", ParentID: r1, Files: []FileToCommit{
		{Name: "a", Content: []byte("a1\n")}, {Name: "b", Content: []byte("b2\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	peer := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return source.HandleSync(ctx, payload)
	})
	hc, err := NewHorizonCone([]string{"a"}, nil, Horizon{Checkin: c1})
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "t.fossil"), peer,
		CloneOpts{StoreCone: hc, PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	_, err = target.ReadFileAt(c2, "b")
	if err == nil || errors.Is(err, ErrBelowHorizon) {
		t.Fatalf("ReadFileAt above C outside the paths: %v, want a refusal not below the horizon", err)
	}
	rec := &gimmeRecorder{t: t, peer: peer}
	_, err = target.ReadFileAtContext(context.Background(), c2, "b", RequiredContentOpts{Peer: rec})
	if err == nil || errors.Is(err, ErrBelowHorizon) || rec.exchanges != 0 {
		t.Fatalf("ReadFileAtContext above C outside the paths: %v after %d exchange(s)", err, rec.exchanges)
	}
}

// A structural read whose peer supplies the manifest but withholds the
// payload fails on the second fetch, holding the manifest and not the payload.
func TestReadFileContextStructuralPayloadWithheld(t *testing.T) {
	f := newHorizonFixture(t)
	target := horizonClone(t, f.peer, f.c2, true)
	before := storeConeString(t, target)
	payload := hash.SHA1([]byte("old a\n"))
	withholding := TransportFunc(func(ctx context.Context, request []byte) ([]byte, error) {
		reply, err := f.source.HandleSync(ctx, request)
		if err != nil {
			return nil, err
		}
		message, err := xfer.Decode(reply, xfer.ContentTypeCompressed)
		if err != nil {
			t.Fatal(err)
		}
		kept := message.Cards[:0]
		for _, card := range message.Cards {
			if c, ok := card.(*xfer.FileCard); ok && c.UUID == payload {
				continue
			}
			if c, ok := card.(*xfer.CFileCard); ok && c.UUID == payload {
				continue
			}
			kept = append(kept, card)
		}
		message.Cards = kept
		return message.Encode()
	})
	_, err := target.ReadFileAtContext(context.Background(), f.c1, "a", RequiredContentOpts{Peer: withholding})
	if err == nil || !strings.Contains(err.Error(), payload) {
		t.Fatalf("withheld payload: %v, want a refusal naming %s", err, payload)
	}
	if _, held, err := content.CheckAvailableByUUID(target.inner.DB(), f.c1); err != nil || !held {
		t.Fatalf("the fetched manifest is not held (%v)", err)
	}
	if holdsBlob(t, target, "old a\n") {
		t.Fatal("the withheld payload is held")
	}
	if after := storeConeString(t, target); after != before {
		t.Fatalf("store cone moved: %q, was %q", after, before)
	}
}

// A read needing more than one demand fetch may carry is refused, naming
// DeepenHorizon, without contacting the peer.
func TestDemandFetchOverLimitNamesDeepen(t *testing.T) {
	f := newHorizonFixture(t)
	target := horizonClone(t, f.peer, f.c2, false)
	if content.DemandFetchLimit != internalsync.MaxGimmeBase {
		t.Fatalf("DemandFetchLimit = %d, want one exchange's gimmes, %d", content.DemandFetchLimit, internalsync.MaxGimmeBase)
	}
	names := make([]string, content.DemandFetchLimit+1)
	for i := range names {
		names[i] = hash.SHA1(fmt.Appendf(nil, "%d", i))
	}
	peer := &gimmeRecorder{t: t, peer: f.peer}
	err := target.demandFetch(context.Background(), RequiredContentOpts{Peer: peer}, func() error {
		return &content.MissingBelowHorizon{Horizon: f.c2, Names: names}
	})
	if !errors.Is(err, ErrBelowHorizon) || !strings.Contains(err.Error(), "DeepenHorizon") {
		t.Fatalf("over-limit demand fetch: %v, want ErrBelowHorizon naming DeepenHorizon", err)
	}
	if peer.exchanges != 0 {
		t.Fatalf("over-limit demand fetch exchanged %d time(s)", peer.exchanges)
	}
}

// zitOnPath is the zit binary, skipping when there is none unless
// REQUIRE_ZIT_BIN=1 makes that a failure, as the conformance interop runs do.
func zitOnPath(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("zit")
	if err != nil {
		if os.Getenv("REQUIRE_ZIT_BIN") == "1" {
			t.Fatal("REQUIRE_ZIT_BIN=1 but no zit binary found")
		}
		t.Skip("zit not found: the interop run needs it")
	}
	return bin
}

// A content-horizon clone of zit's origin demand-fetches a payload below C
// from zit as it does from go-libfossil.
func TestReadFileContextZitOrigin(t *testing.T) {
	bin := zitOnPath(t)
	f := newHorizonFixture(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(f.source.XferHandler())
	defer server.Close()
	zdense := filepath.Join(dir, "zdense")
	zitCmd(t, bin, home, dir, "clone", "--no-open", server.URL, zdense)
	origin := NewHTTPTransport(zitServeURL(t, bin, home, zdense))

	target := horizonClone(t, origin, f.c2, false)
	before := storeConeString(t, target)
	rid, err := target.ResolveVersion(f.c1)
	if err != nil {
		t.Fatal(err)
	}
	peer := &gimmeRecorder{t: t, peer: origin}
	got, err := target.ReadFileContext(context.Background(), rid, "a", RequiredContentOpts{Peer: peer})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old a\n" {
		t.Fatalf("ReadFileContext from zit = %q", got)
	}
	if want := []string{hash.SHA1([]byte("old a\n"))}; !slices.Equal(peer.sorted(), want) {
		t.Fatalf("gimmes = %v, want %v", peer.gimmes, want)
	}
	if after := storeConeString(t, target); after != before {
		t.Fatalf("store cone moved: %q, was %q", after, before)
	}
}

func zitCmd(t *testing.T, bin, home, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home, "FOSSIL_HOME="+home, "USER=u")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("zit %q: %v\n%s", args, err, out)
	}
}

// zitServeURL starts `zit serve` in dir on a free port, stopped at cleanup
// and in any case by the context's deadline.
func zitServeURL(t *testing.T, bin, home, dir string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	cmd := exec.CommandContext(ctx, bin, "serve", "--port", fmt.Sprint(port))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home, "USER=u")
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return "http://" + addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("zit serve did not start")
	return ""
}
