package hash_test

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/hash"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func setupDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.fossil"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.CreateRepoSchema(d); err != nil {
		t.Fatalf("CreateRepoSchema: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func setPolicy(t *testing.T, d *db.DB, value string) {
	t.Helper()
	if _, err := d.Exec(
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES('hash-policy', ?, 0)", value,
	); err != nil {
		t.Fatalf("set hash-policy: %v", err)
	}
}

// insertArtifact adds a blob row named with the given uuid so the sniffing
// paths (policy auto, absent config row) have something to look at.
func insertArtifact(t *testing.T, d *db.DB, uuid string) {
	t.Helper()
	if _, err := d.Exec(
		"INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, 0, x'', 1)", uuid,
	); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
}

const (
	sha1Name = "da39a3ee5e6b4b0d3255bfef95601890afd80709"
	sha3Name = "a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a"
)

// Policy integers are Fossil's own: sha1=0, auto=1, sha3=2, sha3-only=3,
// shun-sha1=4 (verified against fossil 2.28's `fossil hash-policy`).
func TestNamingForPolicy(t *testing.T) {
	tests := []struct {
		name      string
		policy    string
		artifact  string // existing artifact name, "" for an empty repo
		wantAlg   hash.Alg
		wantReuse bool
	}{
		{"sha1", "0", "", hash.AlgSHA1, true},
		{"sha1 ignores existing sha3", "0", sha3Name, hash.AlgSHA1, true},
		{"auto stays sha1 while repo is sha1", "1", sha1Name, hash.AlgSHA1, true},
		{"auto promotes once a sha3 artifact exists", "1", sha3Name, hash.AlgSHA3, true},
		{"sha3", "2", sha1Name, hash.AlgSHA3, true},
		{"sha3-only does not reuse legacy names", "3", sha1Name, hash.AlgSHA3, false},
		{"shun-sha1 does not reuse legacy names", "4", sha1Name, hash.AlgSHA3, false},
		{"garbage value falls back to auto", "nonsense", sha3Name, hash.AlgSHA3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := setupDB(t)
			setPolicy(t, d, tt.policy)
			if tt.artifact != "" {
				insertArtifact(t, d, tt.artifact)
			}
			got := hash.NamingFor(d)
			if got.New != tt.wantAlg {
				t.Errorf("New = %v, want %v", got.New, tt.wantAlg)
			}
			if got.ReuseLegacy != tt.wantReuse {
				t.Errorf("ReuseLegacy = %v, want %v", got.ReuseLegacy, tt.wantReuse)
			}
		})
	}
}

// A repo with no hash-policy row is treated as "auto": legacy repos created
// before the config row existed keep working off their own artifact names.
func TestNamingForAbsentConfigRow(t *testing.T) {
	tests := []struct {
		name     string
		artifact string
		want     hash.Alg
	}{
		{"empty repo defaults to sha1", "", hash.AlgSHA1},
		{"sha1 repo stays sha1", sha1Name, hash.AlgSHA1},
		{"sha3 repo uses sha3", sha3Name, hash.AlgSHA3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := setupDB(t)
			if tt.artifact != "" {
				insertArtifact(t, d, tt.artifact)
			}
			if got := hash.NamingFor(d).New; got != tt.want {
				t.Errorf("New = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAlgHash(t *testing.T) {
	content := []byte("hello fossil world")
	if got, want := hash.AlgSHA1.Hash(content), hash.SHA1(content); got != want {
		t.Errorf("AlgSHA1.Hash = %s, want %s", got, want)
	}
	if got, want := hash.AlgSHA3.Hash(content), hash.SHA3(content); got != want {
		t.Errorf("AlgSHA3.Hash = %s, want %s", got, want)
	}
}

func TestAlgOther(t *testing.T) {
	if got := hash.AlgSHA1.Other(); got != hash.AlgSHA3 {
		t.Errorf("AlgSHA1.Other() = %v, want AlgSHA3", got)
	}
	if got := hash.AlgSHA3.Other(); got != hash.AlgSHA1 {
		t.Errorf("AlgSHA3.Other() = %v, want AlgSHA1", got)
	}
}
