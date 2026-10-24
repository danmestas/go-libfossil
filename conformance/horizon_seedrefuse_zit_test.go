package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/simio"
)

// TestZitHorizonSeedRefuseInterop pushes, from zit, a control artifact
// bearing go-libfossil's seed label to a go-libfossil origin, which refuses
// the whole push with the seed-refused error card (horizon §5.2.3, zit 0.7.1
// erratum). zit 0.7.0 keeps the seed it holds out of every push, so the
// artifact reaches zit as ordinary content: a go-libfossil clone of the
// origin writes it, and zit clones that clone. The reverse direction, a
// zit origin refusing its own label, waits for zit 0.7.1.
func TestZitHorizonSeedRefuseInterop(t *testing.T) {
	bin := zitHorizonBinary(t)
	origin, err := repo.Create(filepath.Join(t.TempDir(), "origin.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	if _, err := origin.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, _, err := manifest.Checkin(origin, manifest.CheckinOpts{Files: []manifest.File{{Name: "c1", Content: []byte("c1\n")}},
		Comment: "c1", User: "u", Time: when}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(&hzRecorder{r: origin})
	defer server.Close()

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	mid, _, err := sync.Clone(context.Background(), filepath.Join(dir, "mid.fossil"),
		&sync.HTTPTransport{URL: server.URL}, sync.CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer mid.Close()
	if _, err := mid.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	var c1 libfossil.FslID
	if err := mid.DB().QueryRow("SELECT objid FROM event WHERE type='ci'").Scan(&c1); err != nil {
		t.Fatal(err)
	}
	labelled, err := tag.AddTag(mid, tag.TagOpts{TargetRID: c1, TagName: "seedtest", TagType: tag.TagSingleton,
		Value: "a", User: seedLoginGo, Time: when.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var labelledUUID string
	if err := mid.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", labelled).Scan(&labelledUUID); err != nil {
		t.Fatal(err)
	}
	midServer := httptest.NewServer(&hzRecorder{r: mid})
	defer midServer.Close()

	zd := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", midServer.URL, zd, "--no-open", "--once")
	out, err := zitCommand(bin, home, zd, "push", server.URL)
	if err == nil || !strings.Contains(out, "seed refused: a seed is private to the clone that holds it; it is never pushed") {
		t.Fatalf("zit's push of a go-libfossil-labelled control artifact: %v\n%s", err, out)
	}
	// The igot round left a phantom for it, which the origin asked for.
	var held bool
	if err := origin.DB().QueryRow("SELECT EXISTS(SELECT 1 FROM blob WHERE uuid=? AND size>=0)", labelledUUID).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("the origin stored the refused artifact")
	}
}
