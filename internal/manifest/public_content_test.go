package manifest

import (
	"bytes"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
)

// Reuse a private blob by bytes and by UUID in flat and tree check-ins, then
// verify publication, sync queuing, and preservation of the original content.
func TestCheckinPublishesReusedPrivateContent(t *testing.T) {
	for _, tree := range []bool{false, true} {
		for _, carried := range []bool{false, true} {
			name := "flat"
			if tree {
				name = "tree"
			}
			if carried {
				name += "/carried"
			} else {
				name += "/stored"
			}
			t.Run(name, func(t *testing.T) {
				r := sha3Repo(t)
				if tree {
					setSettings(t, r, SettingEnableSubmanifests, "on", SettingSubmanifestK, "3")
				}
				files := a1CheckinFiles()
				rid, uuid, err := blob.Store(r.DB(), files[0].Content)
				if err != nil {
					t.Fatal(err)
				}
				if err := content.MakePrivate(r.DB(), int64(rid)); err != nil {
					t.Fatal(err)
				}
				if _, err := r.DB().Exec("DELETE FROM unsent WHERE rid=?", rid); err != nil {
					t.Fatal(err)
				}
				want := bytes.Clone(files[0].Content)
				if carried {
					files[0].UUID, files[0].Content = uuid, nil
				}
				_, d := commit(t, r, CheckinOpts{Files: files, Comment: "publish"})
				if tree && len(d.S) == 0 {
					t.Fatal("fixture did not emit a tree")
				}
				if content.IsPrivate(r.DB(), int64(rid)) {
					t.Error("public check-in left referenced content private")
				}
				var queued int
				if err := r.DB().QueryRow("SELECT count(*) FROM unsent WHERE rid=?", rid).Scan(&queued); err != nil {
					t.Fatal(err)
				}
				if queued != 1 {
					t.Error("referenced content not queued for public sync")
				}
				got, err := blob.Load(r.DB(), rid)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("reused content = %q, %v; want %q", got, err, want)
				}
			})
		}
	}
}

// Abort after file publication but before the check-in completes: neither the
// privacy change nor the sync queue entry may survive the failed transaction.
func TestCheckinRollsBackContentPublication(t *testing.T) {
	for _, carried := range []bool{false, true} {
		name := "stored"
		if carried {
			name = "carried"
		}
		t.Run(name, func(t *testing.T) {
			r := setupTestRepo(t)
			file := File{Name: "private.txt", Content: []byte("private content")}
			rid, uuid, err := blob.Store(r.DB(), file.Content)
			if err != nil {
				t.Fatal(err)
			}
			if err := content.MakePrivate(r.DB(), int64(rid)); err != nil {
				t.Fatal(err)
			}
			if _, err := r.DB().Exec("DELETE FROM unsent WHERE rid=?", rid); err != nil {
				t.Fatal(err)
			}
			if carried {
				file.UUID, file.Content = uuid, nil
			}
			if _, err := r.DB().Exec(`CREATE TRIGGER fail_checkin BEFORE INSERT ON event
				BEGIN SELECT RAISE(ABORT, 'injected check-in failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Checkin(r, CheckinOpts{Files: []File{file}, User: "testuser"}); err == nil {
				t.Fatal("check-in succeeded through injected failure")
			}
			if !content.IsPrivate(r.DB(), int64(rid)) {
				t.Error("failed check-in published private content")
			}
			var queued int
			if err := r.DB().QueryRow("SELECT count(*) FROM unsent WHERE rid=?", rid).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if queued != 0 {
				t.Error("failed check-in queued private content")
			}
		})
	}
}
