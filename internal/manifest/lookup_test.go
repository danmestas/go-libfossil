package manifest

import (
	"testing"
	"time"
)

// LookupFile on a delta manifest made by Checkin: the delta's own cards
// answer first, a deleted name is not found, and an untouched name comes from
// the baseline.
func TestLookupFile_Delta(t *testing.T) {
	r := setupTestRepo(t)
	at := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	first, _, err := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("a")},
			{Name: "b.txt", Content: []byte("b")},
			{Name: "c.txt", Content: []byte("c")},
		},
		Comment: "one", User: "u", Time: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("a2")},
			{Name: "c.txt", Content: []byte("c")},
		},
		Comment: "two", User: "u", Time: at.Add(time.Minute), Parent: first, Delta: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := ListFiles(r, second)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, f := range listed {
		byName[f.Name] = f.UUID
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "missing.txt"} {
		got, found, err := LookupFile(r, second, name)
		if err != nil {
			t.Fatalf("LookupFile(%s): %v", name, err)
		}
		if want, ok := byName[name]; found != ok || got.UUID != want {
			t.Errorf("LookupFile(%s) = %q, %v; ListFiles has %q, %v",
				name, got.UUID, found, want, ok)
		}
	}
}

// A name the delta itself carries resolves with no baseline at all (8.4); this
// is the path annotate, the merge resolver and conflict reads now take.
func TestLookupFileCached_MissingBaseline(t *testing.T) {
	r := setupTestRepo(t)
	at := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	first, _, err := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("a")}, {Name: "b.txt", Content: []byte("b")},
			{Name: "c.txt", Content: []byte("c")}, {Name: "d.txt", Content: []byte("d")},
			{Name: "e.txt", Content: []byte("e")}, {Name: "f.txt", Content: []byte("f")},
		},
		Comment: "one", User: "u", Time: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "a.txt", Content: []byte("a2")}, {Name: "b.txt", Content: []byte("b")},
			{Name: "c.txt", Content: []byte("c")}, {Name: "d.txt", Content: []byte("d")},
			{Name: "e.txt", Content: []byte("e")}, {Name: "f.txt", Content: []byte("f")},
		},
		Comment: "two", User: "u", Time: at.Add(time.Minute), Parent: first, Delta: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := GetManifestCached(r, second, nil); err != nil || d.B == "" {
		t.Fatalf("setup: want a delta manifest, B=%q err=%v", d.B, err)
	}
	if _, err := r.DB().Exec("DELETE FROM blob WHERE rid=?", first); err != nil {
		t.Fatal(err)
	}
	if _, found, err := LookupFileCached(r, second, "a.txt", nil); err != nil || !found {
		t.Fatalf("own F card with the baseline gone: found=%v err=%v", found, err)
	}
	if _, _, err := LookupFileCached(r, second, "b.txt", nil); err == nil {
		t.Fatal("a name that needs the baseline must fail when it is gone")
	}
}
