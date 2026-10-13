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
		Files:   []File{{Name: "a.txt", Content: []byte("a2")}, {Name: "c.txt", Content: []byte("c")}},
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
			t.Errorf("LookupFile(%s) = %q, %v; ListFiles has %q, %v", name, got.UUID, found, want, ok)
		}
	}
}
