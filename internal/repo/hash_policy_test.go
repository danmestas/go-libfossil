package repo_test

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "r.fossil"), "tester", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func storedPolicy(t *testing.T, r *repo.Repo) string {
	t.Helper()
	got, err := r.Config("hash-policy")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	return got
}

// artifact-format-00 7.3: requesting auto once a SHA3 artifact exists stores
// sha3, as fossil's hash-policy command does; without one, auto is stored.
func TestSetHashPolicyCoercesAuto(t *testing.T) {
	for _, tt := range []struct {
		name   string
		before string // policy content is stored under first; "" stores none
		want   string
	}{
		{"empty repo", "", "1"},
		{"sha1 artifact", "sha1", "1"},
		{"sha3 artifact", "sha3", "2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepo(t)
			if tt.before != "" {
				if err := r.SetHashPolicy(tt.before); err != nil {
					t.Fatalf("SetHashPolicy(%q): %v", tt.before, err)
				}
				if _, _, err := blob.Store(r.DB(), []byte("content")); err != nil {
					t.Fatalf("blob.Store: %v", err)
				}
			}
			if err := r.SetHashPolicy("auto"); err != nil {
				t.Fatalf("SetHashPolicy(auto): %v", err)
			}
			if got := storedPolicy(t, r); got != tt.want {
				t.Errorf("stored hash-policy = %q, want %q", got, tt.want)
			}
		})
	}
}

// 7.3 has no demotion guard: sha1 may be set after sha3.
func TestSetHashPolicyNoDemotionGuard(t *testing.T) {
	r := newRepo(t)
	for _, name := range []string{"shun-sha1", "sha1"} {
		if err := r.SetHashPolicy(name); err != nil {
			t.Fatalf("SetHashPolicy(%q): %v", name, err)
		}
	}
	if got := storedPolicy(t, r); got != "0" {
		t.Errorf("stored hash-policy = %q, want 0", got)
	}
}
