package manifest

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
)

func setRepoHashPolicy(t *testing.T, r *repo.Repo, value string) {
	t.Helper()
	if err := r.SetConfig("hash-policy", value); err != nil {
		t.Fatalf("SetConfig(hash-policy): %v", err)
	}
}

// A commit into a sha3 repo writes SHA3 F-cards. Naming them SHA1 gave every
// tracked file a differently-derived hash from the one already in the parent
// manifest, so `fossil diff` reported the whole tree as CHANGED (#223).
func TestCheckinNamesArtifactsByHashPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		alg    func([]byte) string
		width  int
	}{
		{"sha1 policy", "0", hash.SHA1, 40},
		{"sha3 policy", "2", hash.SHA3, 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupTestRepo(t)
			setRepoHashPolicy(t, r, tt.policy)
			content := []byte("file content named by policy")

			rid, uuid, err := Checkin(r, CheckinOpts{
				Files:   []File{{Name: "a.md", Content: content}},
				Comment: "initial commit",
				User:    "testuser",
				Time:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			})
			if err != nil {
				t.Fatalf("Checkin: %v", err)
			}
			if len(uuid) != tt.width {
				t.Errorf("manifest uuid %s is %d chars, want %d", uuid, len(uuid), tt.width)
			}

			d := loadDeck(t, r, rid)
			if len(d.F) != 1 {
				t.Fatalf("F-cards = %d, want 1", len(d.F))
			}
			if want := tt.alg(content); d.F[0].UUID != want {
				t.Errorf("F-card uuid = %s, want %s", d.F[0].UUID, want)
			}
		})
	}
}

// The reported symptom: commit, edit one file, commit again — only the file
// that actually changed gets a new F-card. Every other F-card must carry the
// identical artifact name so a diff shows one changed file, not the tree.
func TestSecondCheckinLeavesUntouchedFCardsAlone(t *testing.T) {
	r := setupTestRepo(t)
	setRepoHashPolicy(t, r, "2")

	files := []File{
		{Name: "a.md", Content: []byte("a\n")},
		{Name: "b.md", Content: []byte("b\n")},
		{Name: "c.md", Content: []byte("c\n")},
	}
	firstRid, _, err := Checkin(r, CheckinOpts{
		Files: files, Comment: "init", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin(first): %v", err)
	}

	edited := []File{
		{Name: "a.md", Content: []byte("a edited\n")},
		files[1],
		files[2],
	}
	secondRid, _, err := Checkin(r, CheckinOpts{
		Files: edited, Parent: firstRid, Comment: "edit a", User: "testuser",
		Time: time.Date(2024, 1, 15, 10, 31, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin(second): %v", err)
	}

	before := fCardsByName(t, r, firstRid)
	after := fCardsByName(t, r, secondRid)
	for name, uuid := range before {
		switch name {
		case "a.md":
			if after[name] == uuid {
				t.Errorf("%s: uuid unchanged after an edit", name)
			}
		default:
			if after[name] != uuid {
				t.Errorf("%s: uuid changed from %s to %s without an edit", name, uuid, after[name])
			}
		}
	}
}

func loadDeck(t *testing.T, r *repo.Repo, rid libfossil.FslID) *deck.Deck {
	t.Helper()
	data, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatalf("content.Expand(rid=%d): %v", rid, err)
	}
	d, err := deck.Parse(data)
	if err != nil {
		t.Fatalf("deck.Parse(rid=%d): %v", rid, err)
	}
	return d
}

func fCardsByName(t *testing.T, r *repo.Repo, rid libfossil.FslID) map[string]string {
	t.Helper()
	byName := make(map[string]string)
	for _, f := range loadDeck(t, r, rid).F {
		byName[f.Name] = f.UUID
	}
	return byName
}
