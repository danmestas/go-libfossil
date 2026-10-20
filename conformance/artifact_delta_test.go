package conformance

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// Delta manifests (Sections 4.7.3 and 8): a baseline and deltas against it
// are stored as artifacts, then enumerated with manifest.ListFiles.

const deltaBaseline = "C baseline\nD " + ts + "\nF a.txt " + h1 + "\nF b.txt " + h2 +
	"\nF c.txt " + h3 + "\nU u\n"

// storeArtifact stores body, with its Z card, and returns its rid and name.
func storeArtifact(t *testing.T, r *repo.Repo, body string) (libfossil.FslID, string) {
	t.Helper()
	rid, uuid, err := blob.Store(r.DB(), withZ(body))
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}
	return rid, uuid
}

// fileSet lists a manifest's full file set as name=hash entries.
func fileSet(r *repo.Repo, rid libfossil.FslID) ([]string, error) {
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		return nil, err
	}
	var set []string
	for _, f := range files {
		set = append(set, f.Name+"="+f.UUID)
	}
	return set, nil
}

// deltaTree stores the baseline and a delta whose cards after the B card
// are rest, and checks the delta's file set.
func deltaTree(rest string, expected ...string) func(*testing.T) error {
	return func(t *testing.T) error {
		r := newPolicyRepo(t, "2")
		_, baseline := storeArtifact(t, r, deltaBaseline)
		rid, _ := storeArtifact(t, r, "B "+baseline+"\n"+rest)
		set, err := fileSet(r, rid)
		if err != nil {
			return err
		}
		return want("file set", set, expected)
	}
}

// deltaFails stores a delta whose B card names what baseline stored, and
// wants enumerating its files to fail.
func deltaFails(baseline func(*testing.T, *repo.Repo) string) func(*testing.T) error {
	return func(t *testing.T) error {
		r := newPolicyRepo(t, "2")
		rid, _ := storeArtifact(t, r, "B "+baseline(t, r)+"\nC delta\nD "+ts+"\nU u\n")
		set, err := fileSet(r, rid)
		if err == nil {
			return expectf("enumerated the file set %v", set)
		}
		return nil
	}
}

func deltaManifestCases() []apiCase {
	return []apiCase{
		{Case{ID: "artifact.8.baseline-tree", Req: "AF-8-1"}, func(t *testing.T) error {
			r := newPolicyRepo(t, "2")
			rid, _ := storeArtifact(t, r, deltaBaseline)
			set, err := fileSet(r, rid)
			if err != nil {
				return err
			}
			return want("file set", set, []string{"a.txt=" + h1, "b.txt=" + h2, "c.txt=" + h3})
		}},
		{Case{ID: "artifact.8.empty-delta", Req: "AF-8-1"}, deltaTree("C delta\nD "+ts+"\nU u\n",
			"a.txt="+h1, "b.txt="+h2, "c.txt="+h3)},
		{Case{ID: "artifact.8.2.delta-tree", Req: "AF-8.2-1"}, deltaTree("C delta\nD "+ts+
			"\nF a.txt "+h4+"\nF c.txt\nF d.txt "+h5+"\nU u\n",
			"a.txt="+h4, "b.txt="+h2, "d.txt="+h5)},
		{Case{ID: "artifact.8.2.unmatched-deletions", Req: "AF-8.2-1"}, deltaTree("C delta\nD "+ts+
			"\nF aa.txt\nF zz.txt\nU u\n", "a.txt="+h1, "b.txt="+h2, "c.txt="+h3)},
		// A rename is a delete of the old name and an add of the new one; an
		// old-name field alone does not remove the baseline's file.
		{Case{ID: "artifact.8.2.rename", Req: "AF-8.2-1"}, deltaTree("C delta\nD "+ts+
			"\nF b.txt\nF b2.txt "+h2+" w b.txt\nU u\n", "a.txt="+h1, "b2.txt="+h2, "c.txt="+h3)},
		{Case{ID: "artifact.8.2.oldname-metadata", Req: "AF-8.2-1"}, deltaTree("C delta\nD "+ts+
			"\nF b2.txt "+h2+" w b.txt\nU u\n",
			"a.txt="+h1, "b.txt="+h2, "b2.txt="+h2, "c.txt="+h3)},
		{Case{ID: "artifact.8.3.missing-baseline", Req: "AF-8.3-1"},
			deltaFails(func(*testing.T, *repo.Repo) string { return h9 })},
		{Case{ID: "artifact.8.3.baseline-not-manifest", Req: "AF-8.3-1"},
			deltaFails(func(t *testing.T, r *repo.Repo) string {
				_, uuid := storeArtifact(t, r, baseControl)
				return uuid
			})},
		// Section 8.4: single-name lookup searches the delta first; a delete
		// marker answers not-found without reading the baseline.
		{Case{ID: "artifact.8.4.delta-entry", Req: "AF-8.4-1"},
			lookup(true, "F a.txt "+h4+"\n", "a.txt", h4, false)},
		{Case{ID: "artifact.8.4.delete-marker", Req: "AF-8.4-1"},
			lookup(true, "F b.txt\n", "b.txt", "", false)},
		{Case{ID: "artifact.8.4.baseline-entry", Req: "AF-8.4-1"},
			lookup(true, "F a.txt "+h4+"\n", "b.txt", h2, false)},
		{Case{ID: "artifact.8.4.delete-marker-baseline-unread", Req: "AF-8.4-1"},
			lookup(false, "F b.txt\n", "b.txt", "", false)},
		{Case{ID: "artifact.8.4.delta-entry-baseline-unread", Req: "AF-8.4-1"},
			lookup(false, "F a.txt "+h4+"\n", "a.txt", h4, false)},
		// Section 8.3: an absent name needs the baseline, and an unresolvable one
		// is a fatal error of the search.
		{Case{ID: "artifact.8.4.absent-baseline-missing", Req: "AF-8.4-1"},
			lookup(false, "F a.txt "+h4+"\n", "b.txt", "", true)},
		// The search stops at the baseline's own F cards: c.txt reaches the
		// baseline only through the delta the baseline names.
		{Case{ID: "artifact.8.4.baseline-chain-not-followed", Req: "AF-8.4-1"},
			func(t *testing.T) error {
				r := newPolicyRepo(t, "2")
				_, baseline := storeArtifact(t, r, deltaBaseline)
				_, first := storeArtifact(t, r, "B "+baseline+"\nC first\nD "+ts+
					"\nF a.txt "+h4+"\nU u\n")
				rid, _ := storeArtifact(t, r, "B "+first+"\nC second\nD "+ts+
					"\nF d.txt "+h5+"\nU u\n")
				return wantLookup(r, rid, "c.txt", "", false)
			}},
		// Section 4.7.3 (-00): a delta naming a delta is accepted, and its
		// file set is built from the named delta's own F cards only, so
		// b.txt and c.txt, which that delta inherits, are absent.
		{Case{ID: "artifact.4.7.3.delta-of-delta-one-level", Req: "AF-4.7.3-1"},
			func(t *testing.T) error {
				r := newPolicyRepo(t, "2")
				_, baseline := storeArtifact(t, r, deltaBaseline)
				_, first := storeArtifact(t, r, "B "+baseline+"\nC first\nD "+ts+
					"\nF a.txt "+h4+"\nU u\n")
				rid, _ := storeArtifact(t, r, "B "+first+"\nC second\nD "+ts+
					"\nF d.txt "+h5+"\nU u\n")
				set, err := fileSet(r, rid)
				if err != nil {
					return err
				}
				return want("file set", set, []string{"a.txt=" + h4, "d.txt=" + h5})
			}},
		// Sections 4.7.3 and 8.2: the named delta's delete marker removes
		// b.txt from the merged set; it must not surface as a hashless entry.
		{Case{ID: "artifact.4.7.3.delta-of-delta-baseline-delete", Req: "AF-4.7.3-2"},
			func(t *testing.T) error {
				r := newPolicyRepo(t, "2")
				_, baseline := storeArtifact(t, r, deltaBaseline)
				_, first := storeArtifact(t, r, "B "+baseline+"\nC first\nD "+ts+
					"\nF b.txt\nU u\n")
				rid, _ := storeArtifact(t, r, "B "+first+"\nC second\nD "+ts+
					"\nF d.txt "+h5+"\nU u\n")
				set, err := fileSet(r, rid)
				if err != nil {
					return err
				}
				return want("file set", set, []string{"d.txt=" + h5})
			}},
		// The same marker answers a lookup of b.txt: not found, and no
		// hashless entry.
		{Case{ID: "artifact.4.7.3.delta-of-delta-baseline-delete-lookup", Req: "AF-4.7.3-2"},
			func(t *testing.T) error {
				r := newPolicyRepo(t, "2")
				_, baseline := storeArtifact(t, r, deltaBaseline)
				_, first := storeArtifact(t, r, "B "+baseline+"\nC first\nD "+ts+
					"\nF b.txt\nU u\n")
				rid, _ := storeArtifact(t, r, "B "+first+"\nC second\nD "+ts+
					"\nF d.txt "+h5+"\nU u\n")
				return wantLookup(r, rid, "b.txt", "", false)
			}},
	}
}

// Crosslink refuses a B card whose target is not a check-in the same way
// ListFiles does: the delta gets no mlink rows, where a valid baseline gives it
// the baseline's files. The files exist, so a refusal cannot be mistaken for a
// check-in deferred on a missing blob. draft-fossil-artifact-format-00 Section
// 4.7.3 has a reader accept a delta manifest as the target, resolved one level,
// so only a control artifact is refused.
func TestCrosslinkBaselineMustBeCheckin(t *testing.T) {
	tests := []struct {
		name     string
		baseline func(t *testing.T, r *repo.Repo, file string) string
		wantRows bool
	}{
		{"baseline manifest", func(t *testing.T, r *repo.Repo, file string) string {
			_, uuid := storeArtifact(t, r, "C baseline\nD "+ts+"\nF a.txt "+file+"\nU u\n")
			return uuid
		}, true},
		{"delta manifest", func(t *testing.T, r *repo.Repo, file string) string {
			_, baseline := storeArtifact(t, r, "C baseline\nD "+ts+"\nF a.txt "+file+"\nU u\n")
			_, uuid := storeArtifact(t, r, "B "+baseline+"\nC first\nD "+ts+"\nU u\n")
			return uuid
		}, true},
		{"control artifact", func(t *testing.T, r *repo.Repo, _ string) string {
			_, uuid := storeArtifact(t, r, baseControl)
			return uuid
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newPolicyRepo(t, "2")
			_, file, err := blob.Store(r.DB(), []byte("file content\n"))
			if err != nil {
				t.Fatal(err)
			}
			rid, _ := storeArtifact(t, r, "B "+tt.baseline(t, r, file)+"\nC delta\nD "+ts+"\nF b.txt "+file+"\nU u\n")
			if _, err := manifest.Crosslink(r); err != nil {
				t.Fatalf("Crosslink: %v", err)
			}
			for _, table := range []string{"mlink", "event"} {
				col := map[string]string{"mlink": "mid", "event": "objid"}[table]
				var n int
				if err := r.DB().QueryRow("SELECT count(*) FROM "+table+" WHERE "+col+"=?",
					rid).Scan(&n); err != nil {
					t.Fatal(err)
				}
				// A baseline that is not a manifest holds the whole delta back, as
				// fossil's fetch_baseline does, not only its file rows.
				if (n > 0) != tt.wantRows {
					t.Fatalf("%s rows for the delta = %d, want rows: %v", table, n, tt.wantRows)
				}
			}
		})
	}
}

// lookup stores the deltaBaseline and a delta with the cards rest, or, when
// baselinePresent is false, a delta whose baseline was never stored, then
// looks name up in the delta.
func lookup(baselinePresent bool, rest, name, uuid string, wantErr bool) func(*testing.T) error {
	return func(t *testing.T) error {
		r := newPolicyRepo(t, "2")
		_, baseline := storeArtifact(t, r, deltaBaseline)
		if !baselinePresent {
			baseline = h9
		}
		rid, _ := storeArtifact(t, r, "B "+baseline+"\nC delta\nD "+ts+"\n"+rest+"U u\n")
		return wantLookup(r, rid, name, uuid, wantErr)
	}
}

// wantLookup checks manifest.LookupFile: the hash it finds (empty for
// not-found) or, when wantErr, that the search fails.
func wantLookup(r *repo.Repo, rid libfossil.FslID, name, uuid string, wantErr bool) error {
	entry, found, err := manifest.LookupFile(r, rid, name)
	if wantErr {
		if err == nil {
			return expectf("looking up %q gave found=%v without an error", name, found)
		}
		return nil
	}
	if err != nil {
		return expectf("looking up %q failed: %v", name, err)
	}
	if found != (uuid != "") {
		return expectf("looking up %q gave found=%v, want %v", name, found, uuid != "")
	}
	return want("hash of "+name, entry.UUID, uuid)
}
