package blob

import (
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/hash"
)

func setHashPolicy(t *testing.T, d *db.DB, value string) {
	t.Helper()
	if _, err := d.Exec(
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES('hash-policy', ?, 0)", value,
	); err != nil {
		t.Fatalf("set hash-policy: %v", err)
	}
}

// Store names new artifacts with the algorithm the repo's hash-policy
// selects. Naming everything SHA1 in a sha3 repo is what made every tracked
// file read as CHANGED (#223).
func TestStoreHonoursHashPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		want   func([]byte) string
	}{
		{"sha1 policy", "0", hash.SHA1},
		{"sha3 policy", "2", hash.SHA3},
		{"sha3-only policy", "3", hash.SHA3},
		{"shun-sha1 policy", "4", hash.SHA3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := setupTestDB(t)
			setHashPolicy(t, d, tt.policy)
			content := []byte("content named by policy")

			_, uuid, err := Store(d, content)
			if err != nil {
				t.Fatalf("Store: %v", err)
			}
			if want := tt.want(content); uuid != want {
				t.Errorf("uuid = %s, want %s", uuid, want)
			}
		})
	}
}

// StoreDelta names its artifact the same way Store does — it writes a blob
// row under a content hash exactly like the full-content path.
func TestStoreDeltaHonoursHashPolicy(t *testing.T) {
	d := setupTestDB(t)
	setHashPolicy(t, d, "2")

	srcRid, _, err := Store(d, []byte("the source content, long enough to delta against"))
	if err != nil {
		t.Fatalf("Store(source): %v", err)
	}
	content := []byte("the source content, long enough to delta against, plus a tail")

	_, uuid, err := StoreDelta(d, content, srcRid)
	if err != nil {
		t.Fatalf("StoreDelta: %v", err)
	}
	if want := hash.SHA3(content); uuid != want {
		t.Errorf("uuid = %s, want %s", uuid, want)
	}
}

// A repo whose artifacts are SHA1-named but whose policy says sha3 must
// still recognise unchanged content by its legacy name. Re-storing it under
// a fresh SHA3 name would duplicate the blob and churn the F-card — the same
// every-file-CHANGED symptom, mirrored.
func TestStoreReusesLegacyNameUnderSHA3Policy(t *testing.T) {
	d := setupTestDB(t)
	setHashPolicy(t, d, "0")
	content := []byte("content stored before the repo moved to sha3")

	sha1Rid, sha1UUID, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store(sha1 policy): %v", err)
	}
	if sha1UUID != hash.SHA1(content) {
		t.Fatalf("setup: uuid = %s, want the SHA1 name", sha1UUID)
	}

	setHashPolicy(t, d, "2")
	rid, uuid, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store(sha3 policy): %v", err)
	}
	if rid != sha1Rid || uuid != sha1UUID {
		t.Errorf("Store = (%d, %s), want the existing (%d, %s)", rid, uuid, sha1Rid, sha1UUID)
	}
}

// sha3-only means what it says: the legacy SHA1 artifact is not reused, so
// the content is stored again under its SHA3 name.
func TestStoreDoesNotReuseLegacyNameUnderSHA3Only(t *testing.T) {
	d := setupTestDB(t)
	setHashPolicy(t, d, "0")
	content := []byte("content stored before the repo moved to sha3-only")

	sha1Rid, _, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store(sha1 policy): %v", err)
	}

	setHashPolicy(t, d, "3")
	rid, uuid, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store(sha3-only policy): %v", err)
	}
	if rid == sha1Rid {
		t.Errorf("rid = %d, want a new row rather than the legacy SHA1 artifact", rid)
	}
	if want := hash.SHA3(content); uuid != want {
		t.Errorf("uuid = %s, want %s", uuid, want)
	}
}

// The empty artifact is the one blob every repo has; it must be named by
// policy too, not pinned to SHA1's da39a3ee...
func TestStoreEmptyContentHonoursHashPolicy(t *testing.T) {
	d := setupTestDB(t)
	setHashPolicy(t, d, "2")

	_, uuid, err := Store(d, nil)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if want := hash.SHA3([]byte{}); uuid != want {
		t.Errorf("uuid = %s, want %s", uuid, want)
	}
}

// A phantom under the primary name does not hide a stored blob under the
// legacy one: the content is reused, and the phantom stays a phantom, as in
// fossil's content_put_ex.
func TestStoreReusesLegacyNameBesideAPrimaryPhantom(t *testing.T) {
	d := setupTestDB(t)
	content := []byte("content held under its SHA1 name")
	setHashPolicy(t, d, "0")
	legacyRid, _, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store under sha1: %v", err)
	}
	setHashPolicy(t, d, "2")
	phantomRid, err := StorePhantom(d, hash.SHA3(content))
	if err != nil {
		t.Fatalf("StorePhantom: %v", err)
	}

	rid, name, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store under sha3: %v", err)
	}
	if rid != legacyRid || name != hash.SHA1(content) {
		t.Fatalf("Store = rid %d name %s, want the stored SHA1 blob rid %d", rid, name, legacyRid)
	}
	if !isPhantom(d, phantomRid) {
		t.Errorf("the SHA3 phantom was filled; want it left alone")
	}
}
