package conformance

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/content"
)

// The interop cases run whole sessions between go-libfossil and fossil(1),
// each side serving or cloning the other. They need fossil(1); CI has it.

// fossilTransport is a libfossil.FramedTransport whose far end is fossil(1)
// serving the repository at path, one `fossil http` per round.
type fossilTransport struct {
	ref  *fossilReference
	path string
}

func (t fossilTransport) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	reply, _, err := t.RoundTripFramed(ctx, payload)
	return reply, err
}

func (t fossilTransport) RoundTripFramed(_ context.Context,
	payload []byte) ([]byte, string, error) {
	status, ctype, raw, err := t.ref.serveEntity(t.path, "POST", "/", typeCompressed, payload)
	if err == nil && status != 200 {
		err = expectf("fossil http answered %d: %.200q", status, raw)
	}
	return raw, ctype, err
}

type syncInteropCase struct {
	Case
	run func(fx *syncFixture, ref *fossilReference, dir string) error
}

func syncInteropCases() []syncInteropCase {
	return []syncInteropCase{
		{Case{ID: "sync.7.2.interop-clone-from-fossil", Req: "SP-7.2-1"}, cloneFromFossil},
		{Case{ID: "sync.8.2.interop-fossil-clones-us", Req: "SP-8.2-1"}, fossilClonesUs},
		{Case{ID: "sync.3.7.interop-roundtrip", Req: "SP-3.7-1"}, syncBothWays},
	}
}

func TestSyncInterop(t *testing.T) {
	ref := newFossilReference(t)
	if ref == nil {
		t.Skip("fossil not found")
	}
	top := t.TempDir()
	fx := newSyncFixture(t, top)
	for _, c := range syncInteropCases() {
		c.Case = markSync(c.Case)
		check(t, c.Case, func() error {
			dir, err := os.MkdirTemp(top, "case")
			if err != nil {
				return err
			}
			return c.run(fx, ref, dir)
		})
	}
}

// cloneFromFossil clones F, served by fossil(1), with go-libfossil's Clone.
func cloneFromFossil(fx *syncFixture, ref *fossilReference, dir string) error {
	src, err := prepare(fx, dir, "served.fossil", nil)
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, "clone.fossil")
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	peer := fossilTransport{ref: ref, path: src}
	r, _, err := libfossil.Clone(ctx, dst, peer, libfossil.CloneOpts{})
	if err != nil {
		return expectf("clone from fossil(1): %v", err)
	}
	if err := r.Close(); err != nil {
		return err
	}
	return holdsAll(dst, src)
}

// fossilClonesUs clones F, served by go-libfossil's handler, with
// fossil(1)'s clone.
func fossilClonesUs(fx *syncFixture, ref *fossilReference, dir string) error {
	src, err := prepare(fx, dir, "served.fossil", nil)
	if err != nil {
		return err
	}
	r, err := libfossil.Open(src)
	if err != nil {
		return err
	}
	srv := httptest.NewServer(r.XferHandler())
	dst := filepath.Join(dir, "fossil-clone.fossil")
	_, cloneErr := ref.run(dir, "clone", srv.URL+"/", dst)
	srv.Close()
	if err := r.Close(); err != nil {
		return err
	}
	if cloneErr != nil {
		return expectf("fossil clone: %v", cloneErr)
	}
	return holdsAll(dst, src)
}

// syncBothWays adds a check-in to each of two copies of F, then syncs
// go-libfossil's copy with fossil(1) serving the other: each must end
// with the other's check-in.
func syncBothWays(fx *syncFixture, ref *fossilReference, dir string) error {
	ours, err := prepareCheckin(fx, dir, "ours.fossil", "b.txt")
	if err != nil {
		return err
	}
	theirs, err := prepareCheckin(fx, dir, "theirs.fossil", "c.txt")
	if err != nil {
		return err
	}
	r, err := libfossil.Open(ours)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	_, syncErr := r.Sync(ctx, fossilTransport{ref: ref, path: theirs},
		libfossil.SyncOpts{Push: true, Pull: true})
	if err := r.Close(); err != nil {
		return err
	}
	if syncErr != nil {
		return expectf("sync with fossil(1): %v", syncErr)
	}
	if err := holdsAll(ours, theirs); err != nil {
		return err
	}
	return holdsAll(theirs, ours)
}

// prepareCheckin copies F and commits one new file to the copy.
func prepareCheckin(fx *syncFixture, dir, name, file string) (string, error) {
	path, err := prepare(fx, dir, name, nil)
	if err != nil {
		return "", err
	}
	r, err := libfossil.Open(path)
	if err != nil {
		return "", err
	}
	_, _, err = r.Commit(libfossil.CommitOpts{
		Files:   []libfossil.FileToCommit{{Name: file, Content: []byte(file + "\n")}},
		Comment: "add " + file, User: "setup", Time: time.Unix(syncEpoch+3600, 0),
	})
	if closeErr := r.Close(); err == nil {
		err = closeErr
	}
	return path, err
}

// holdsAll checks the repository at path can reconstruct every public
// artifact of the one at from, to the same bytes.
func holdsAll(path, from string) error {
	src, err := libfossil.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := libfossil.Open(path)
	if err != nil {
		return err
	}
	defer dst.Close()
	rows, err := src.DB().Query("SELECT uuid FROM blob WHERE size>=0" +
		" AND NOT EXISTS(SELECT 1 FROM private WHERE rid=blob.rid)")
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, name := range names {
		if err := sameArtifact(src, dst, name); err != nil {
			return err
		}
	}
	return nil
}

func sameArtifact(src, dst *libfossil.Repo, name string) error {
	want, err := expandName(src, name)
	if err != nil {
		return err
	}
	got, err := expandName(dst, name)
	if err != nil {
		return expectf("%s: %v", filepath.Base(dst.Path()), err)
	}
	if !bytes.Equal(got, want) {
		return expectf("%s: %s differs", filepath.Base(dst.Path()), name)
	}
	return nil
}

func expandName(r *libfossil.Repo, name string) ([]byte, error) {
	rid, ok := content.AvailableByUUID(r.DB(), name)
	if !ok {
		return nil, expectf("%s is not available", name)
	}
	return content.Expand(r.DB(), rid)
}
