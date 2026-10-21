package cli

import (
	"path/filepath"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// Raw and expanded artifact reads share local-only absence diagnostics rather
// than exposing an expansion error for an intentionally unavailable payload.
func TestRepoCatRequiredPayloadRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.fossil")
	r, err := libfossil.Create(path, libfossil.CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = r.Commit(libfossil.CommitOpts{User: "u", Files: []libfossil.FileToCommit{
		{Name: "a.txt", Content: []byte("a")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var uuid string
	if err := r.Inner().DB().QueryRow(`SELECT uuid FROM blob WHERE rid IN
		(SELECT fid FROM mlink WHERE fnid IN
		(SELECT fnid FROM filename WHERE name='a.txt'))`).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Inner().DB().Exec(
		"UPDATE blob SET size=-1, content=NULL WHERE uuid=?", uuid); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []bool{false, true} {
		err := (&RepoCatCmd{Artifact: uuid, Raw: raw}).Run(&Globals{Repo: path})
		if err == nil || !strings.Contains(err.Error(), "widen/deepen") {
			t.Fatalf("raw=%v: inaccurate refusal: %v", raw, err)
		}
	}
}
