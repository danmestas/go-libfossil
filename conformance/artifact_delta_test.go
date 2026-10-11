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
		{Case{ID: "artifact.4.7.3.delta-baseline-invalid", Req: "AF-4.7.3-1"},
			deltaFails(func(t *testing.T, r *repo.Repo) string {
				_, baseline := storeArtifact(t, r, deltaBaseline)
				_, uuid := storeArtifact(t, r, "B "+baseline+"\nC first\nD "+ts+
					"\nF a.txt "+h4+"\nU u\n")
				return uuid
			})},
	}
}
