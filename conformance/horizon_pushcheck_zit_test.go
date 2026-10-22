package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/branch"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/simio"
)

// TestZitHorizonPushCheckInterop runs the push-time cancellation check of
// draft-fossil-horizon-00 §5.7 between go-libfossil and zit both ways over
// trunk c1 -> c4. Each client creates a branch from c4 believing c4 is on trunk
// after the origin moved c4 to branch moved, so its check-in cancels
// sym-trunk: zit's push to go-libfossil's handler and go-libfossil's push to
// `zit serve` are both refused, naming the stale seed and the parent's
// branch. go-libfossil then pushes the correcting control artifact with the
// check-in, and zit admits it.
func TestZitHorizonPushCheckInterop(t *testing.T) {
	bin := zitHorizonBinary(t)
	// zit 9bf7ebfe0f's commit asserts on a SHA1-named tree
	// (src/cmd/checkout.zig:3165), so this origin names by SHA3.
	origin, err := repo.Create(filepath.Join(t.TempDir(), "origin.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	if _, err := origin.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	commit := func(name string, parent libfossil.FslID) libfossil.FslID {
		when = when.Add(time.Hour)
		rid, _, err := manifest.Checkin(origin, manifest.CheckinOpts{Files: []manifest.File{{Name: name, Content: []byte(name + "\n")}},
			Comment: name, User: "u", Parent: parent, Time: when})
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	c4rid := commit("c4", commit("c1", 0))
	c4 := hzUUID(t, origin, c4rid)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(&hzRecorder{r: origin})
	defer server.Close()

	// Every client takes its copy while c4 is still on trunk.
	zc := filepath.Join(dir, "zcheckout")
	zitRun(t, bin, home, dir, "clone", server.URL, zc)
	zitRun(t, bin, home, zc, "update", "--no-sync", c4)
	zd := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", server.URL, zd, "--no-open", "--once")
	peer := &sync.HTTPTransport{URL: zitServe(t, bin, home, zd)}
	r, _, err := sync.Clone(context.Background(), filepath.Join(dir, "dense.fossil"), peer, sync.CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, m := range []struct {
		name  string
		typ   int
		value string
	}{{"branch", tag.TagPropagating, "moved"}, {"sym-moved", tag.TagPropagating, ""}, {"sym-trunk", tag.TagCancel, ""}} {
		when = when.Add(time.Hour)
		if _, err := tag.AddTag(origin, tag.TagOpts{TargetRID: c4rid, TagName: m.name, TagType: m.typ, Value: m.value,
			User: "u", Time: when}); err != nil {
			t.Fatal(err)
		}
	}
	zitRun(t, bin, home, zd, "pull", server.URL)

	// zit pushes to go-libfossil.
	if err := os.WriteFile(filepath.Join(zc, "nb"), []byte("nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zitRun(t, bin, home, zc, "add", "nb")
	zitRun(t, bin, home, zc, "commit", "--no-sync", "--branch", "nb", "-m", "nb")
	before := hzCheckins(t, origin)
	out, err := zitCommand(bin, home, zc, "push", server.URL)
	if !strings.Contains(out, "stale seed") || !strings.Contains(out, "'moved'") {
		t.Fatalf("zit's push of a stale cancellation to go-libfossil: %v\n%s", err, out)
	}
	if after := hzCheckins(t, origin); after != before {
		t.Fatalf("go-libfossil stored the refused push: %d check-ins, was %d", after, before)
	}

	// go-libfossil pushes to zit.
	rc4, err := hzRID(r, c4)
	if err != nil {
		t.Fatal(err)
	}
	when = when.Add(24 * time.Hour)
	nb, _, err := branch.Create(r, branch.CreateOpts{Name: "nb", Parent: rc4, User: "u", Time: when})
	if err != nil {
		t.Fatal(err)
	}
	res, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Push: true})
	if !hzRefusedStale(err, res, "'moved'") {
		t.Fatalf("go-libfossil's push of a stale cancellation to zit: %v %+v", err, res)
	}
	nbUUID := hzUUID(t, r, nb)
	if out, err := zitCommand(bin, home, zd, "info", nbUUID); err == nil {
		t.Fatalf("zit stored the refused check-in:\n%s", out)
	}
	if _, err := tag.AddTag(r, tag.TagOpts{TargetRID: nb, TagName: "sym-moved", TagType: tag.TagCancel,
		User: "u", Time: when.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	res, err = sync.Sync(context.Background(), r, peer, sync.SyncOpts{Push: true})
	if err != nil || len(res.Errors) != 0 {
		t.Fatalf("go-libfossil's push with the correction: %v %q", err, res.Errors)
	}
	zitRun(t, bin, home, zd, "info", nbUUID)
}

// hzCheckins counts the check-ins r holds.
func hzCheckins(t *testing.T, r *repo.Repo) int {
	t.Helper()
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// hzRefusedStale reports a sync refused with a stale-seed diagnostic naming
// want, whether it surfaced as the error or among the result's errors.
func hzRefusedStale(err error, res *sync.SyncResult, want string) bool {
	var texts []string
	if err != nil {
		texts = append(texts, err.Error())
	}
	if res != nil {
		texts = append(texts, res.Errors...)
	}
	all := strings.Join(texts, "\n")
	return strings.Contains(all, "stale seed") && strings.Contains(all, want)
}
