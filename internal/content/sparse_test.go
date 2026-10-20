package content

import (
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
)

func mustCone(t *testing.T, include, exclude []string) *cone.Cone {
	t.Helper()
	c, err := cone.New(include, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStoreConeAbsentIsDense(t *testing.T) {
	d := setupTestDB(t)
	got, err := StoreCone(d)
	if err != nil || got != nil {
		t.Fatalf("StoreCone = %v, %v; want nil, nil", got, err)
	}
}

func TestStoreConeRoundTrip(t *testing.T) {
	d := setupTestDB(t)
	want := mustCone(t, []string{"src", "README"}, []string{"src/vendor"})
	if err := SetStoreCone(d, want); err != nil {
		t.Fatal(err)
	}
	got, err := StoreCone(d)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Equal(want) {
		t.Fatalf("StoreCone = %v, want %v", got, want)
	}
	// Replacing it keeps one row.
	next := mustCone(t, []string{"docs"}, nil)
	if err := SetStoreCone(d, next); err != nil {
		t.Fatal(err)
	}
	if got, _ := StoreCone(d); got == nil || !got.Equal(next) {
		t.Fatalf("after replace StoreCone = %v, want %v", got, next)
	}
	// nil returns the store to dense and leaves no table behind, so a
	// repository that was sparse once is shaped like any dense one.
	if err := SetStoreCone(d, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := StoreCone(d); err != nil || got != nil {
		t.Fatalf("after clear StoreCone = %v, %v; want nil, nil", got, err)
	}
	var n int
	if err := d.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='storecone'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("storecone table left behind by SetStoreCone(nil)")
	}
}

// A cone this implementation cannot read is policy loss (§3.1, §5.2): the
// store reads as dense, never as an error.
func TestStoreConeUnreadableIsDense(t *testing.T) {
	d := setupTestDB(t)
	if err := SetStoreCone(d, mustCone(t, []string{"src"}, nil)); err != nil {
		t.Fatal(err)
	}
	// The first is a later version's lead-in: 0x00, then a version byte this
	// implementation does not know.
	for _, bad := range [][]byte{{0, 9, 1, 0, 3, 0, 's', 'r', 'c'}, {}, []byte("src")} {
		if _, err := d.Exec("UPDATE storecone SET cone=?", bad); err != nil {
			t.Fatal(err)
		}
		got, err := StoreCone(d)
		if err != nil || got != nil {
			t.Fatalf("StoreCone over %q = %v, %v; want nil, nil", bad, got, err)
		}
	}
}

// absenceFixture is a store with one held blob and phantoms in every class.
type absenceFixture struct {
	d                                          *db.DB
	held, out, in, both, none, shunned, source libfossil.FslID
	renamed                                    libfossil.FslID
}

func newAbsenceFixture(t *testing.T) *absenceFixture {
	t.Helper()
	d := setupTestDB(t)
	f := &absenceFixture{d: d}
	phantom := func(name string) libfossil.FslID {
		rid, err := blob.StorePhantom(d, hash.SHA1([]byte(name)))
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	var err error
	if f.held, _, err = blob.Store(d, []byte("held content")); err != nil {
		t.Fatal(err)
	}
	f.out, f.in, f.both = phantom("out"), phantom("in"), phantom("both")
	f.none, f.shunned, f.source = phantom("none"), phantom("shunned"), phantom("source")
	f.renamed = phantom("renamed")
	if _, err := d.Exec("INSERT INTO shun(uuid, mtime) VALUES(?, 0)", hash.SHA1([]byte("shunned"))); err != nil {
		t.Fatal(err)
	}
	// The held blob is a delta over source, so source cannot be elided:
	// the held artifact is unreadable without it.
	if _, err := d.Exec("INSERT INTO delta(rid, srcid) VALUES(?, ?)", f.held, f.source); err != nil {
		t.Fatal(err)
	}
	fnid := map[string]int64{}
	for i, name := range []string{"src/a.c", "tools/t.c", "tools/u.c", "docs/d.txt", "src/b.c"} {
		fnid[name] = int64(i + 1)
		if _, err := d.Exec("INSERT INTO filename(fnid, name) VALUES(?, ?)", fnid[name], name); err != nil {
			t.Fatal(err)
		}
	}
	link := func(fid, pid libfossil.FslID, name, prior string) {
		t.Helper()
		if _, err := d.Exec("INSERT INTO mlink(mid, fid, pmid, pid, fnid, pfnid, mperm) VALUES(100, ?, 0, ?, ?, ?, 0)",
			fid, pid, fnid[name], fnid[prior]); err != nil {
			t.Fatal(err)
		}
	}
	link(f.out, 0, "tools/t.c", "")
	link(f.in, 0, "src/a.c", "")
	link(f.both, 0, "tools/u.c", "")
	link(f.both, 0, "src/b.c", "")
	link(f.shunned, 0, "tools/t.c", "")
	link(f.source, 0, "tools/t.c", "")
	// renamed is named only as the parent version of a rename out of
	// src/b.c: the parent's F card put it in the cone.
	link(f.held, f.renamed, "docs/d.txt", "src/b.c")
	return f
}

func TestAbsencesWithoutForumTable(t *testing.T) {
	f := newAbsenceFixture(t)
	if _, err := f.d.Exec("DROP TABLE forumpost"); err != nil {
		t.Fatal(err)
	}
	if err := SetStoreCone(f.d, mustCone(t, []string{"src"}, nil)); err != nil {
		t.Fatal(err)
	}
	a, err := LoadAbsences(f.d)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Class(f.out); got != Elided {
		t.Fatalf("file in repository without forum table = %v, want elided", got)
	}
}

func TestAbsencesRetainedDeltaChainStaysTransit(t *testing.T) {
	f := newAbsenceFixture(t)
	if err := SetStoreCone(f.d, mustCone(t, []string{"src"}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Exec("UPDATE delta SET srcid=? WHERE rid=?", f.out, f.held); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Exec("INSERT INTO delta(rid, srcid) VALUES(?, ?)", f.out, f.source); err != nil {
		t.Fatal(err)
	}
	a, err := LoadAbsences(f.d)
	if err != nil {
		t.Fatal(err)
	}
	for _, rid := range []libfossil.FslID{f.out, f.source} {
		if got := a.Class(rid); got != Transit {
			t.Errorf("retained delta ancestor %d = %v, want transit", rid, got)
		}
	}
}

func TestAbsencesStructuralReferencesStayTransit(t *testing.T) {
	for name, stmt := range map[string]string{
		"event":         "INSERT INTO event(objid, type) VALUES(?, 'w')",
		"manifest":      "INSERT INTO mlink(mid) VALUES(?)",
		"parent":        "INSERT INTO plink(pid, cid) VALUES(?, 999)",
		"baseline":      "INSERT INTO orphan(rid, baseline) VALUES(999, ?)",
		"tag source":    "INSERT INTO tagxref(tagid, srcid, rid) VALUES(1, ?, 999)",
		"attachment":    "INSERT INTO attachment(attachid, src) SELECT 999, uuid FROM blob WHERE rid=?",
		"forum post":    "INSERT INTO forumpost(fpid) VALUES(?)",
		"forum root":    "INSERT INTO forumpost(fpid, froot) VALUES(999, ?)",
		"forum prior":   "INSERT INTO forumpost(fpid, fprev) VALUES(999, ?)",
		"forum reply":   "INSERT INTO forumpost(fpid, firt) VALUES(999, ?)",
		"cherry parent": "INSERT INTO cherrypick(parentid, childid) VALUES(?, 999)",
		"cherry child":  "INSERT INTO cherrypick(parentid, childid) VALUES(999, ?)",
	} {
		t.Run(name, func(t *testing.T) {
			f := newAbsenceFixture(t)
			if err := SetStoreCone(f.d, mustCone(t, []string{"src"}, nil)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.d.Exec(stmt, f.out); err != nil {
				t.Fatal(err)
			}
			a, err := LoadAbsences(f.d)
			if err != nil {
				t.Fatal(err)
			}
			if got := a.Class(f.out); got != Transit {
				t.Fatalf("structural artifact also named outside cone = %v, want transit", got)
			}
		})
	}
}

func TestAbsencesDense(t *testing.T) {
	f := newAbsenceFixture(t)
	a, err := LoadAbsences(f.d)
	if err != nil {
		t.Fatal(err)
	}
	if a.Cone() != nil {
		t.Fatal("dense store has a cone")
	}
	for rid, want := range map[libfossil.FslID]Absence{
		f.held: Held, f.out: Transit, f.in: Transit, f.both: Transit, f.none: Transit,
		f.shunned: Shunned, f.source: Transit, f.renamed: Transit,
	} {
		if got := a.Class(rid); got != want {
			t.Errorf("dense Class(%d) = %v, want %v", rid, got, want)
		}
	}
	if a.Count(Elided) != 0 || a.Count(Transit) != 6 || a.Count(Shunned) != 1 {
		t.Errorf("dense counts transit=%d shunned=%d elided=%d", a.Count(Transit), a.Count(Shunned), a.Count(Elided))
	}
}

func TestAbsencesSparse(t *testing.T) {
	f := newAbsenceFixture(t)
	if err := SetStoreCone(f.d, mustCone(t, []string{"src"}, nil)); err != nil {
		t.Fatal(err)
	}
	a, err := LoadAbsences(f.d)
	if err != nil {
		t.Fatal(err)
	}
	for rid, want := range map[libfossil.FslID]Absence{
		f.held:    Held,
		f.out:     Elided,  // named only outside the cone
		f.in:      Transit, // named inside it
		f.both:    Transit, // an in-cone name keeps it wanted
		f.none:    Transit, // named by no F card: nothing proves it out of cone
		f.shunned: Shunned,
		f.source:  Transit, // a held delta's base
		f.renamed: Transit, // named in the cone by the parent's F card
	} {
		if got := a.Class(rid); got != want {
			t.Errorf("Class(%d) = %v, want %v", rid, got, want)
		}
	}
	if a.Count(Elided) != 1 || a.Count(Transit) != 5 {
		t.Errorf("counts transit=%d elided=%d", a.Count(Transit), a.Count(Elided))
	}

	// Elision is recomputed from the cone, never stored: widening makes the
	// phantom wanted again, and losing the cone makes every one wanted.
	if err := SetStoreCone(f.d, mustCone(t, []string{"src", "tools"}, nil)); err != nil {
		t.Fatal(err)
	}
	if a, err = LoadAbsences(f.d); err != nil {
		t.Fatal(err)
	}
	if got := a.Class(f.out); got != Transit {
		t.Errorf("after widening Class(out) = %v, want transit", got)
	}
	if err := SetStoreCone(f.d, mustCone(t, []string{"docs"}, nil)); err != nil {
		t.Fatal(err)
	}
	if a, err = LoadAbsences(f.d); err != nil {
		t.Fatal(err)
	}
	if got := a.Class(f.in); got != Elided {
		t.Errorf("after narrowing Class(in) = %v, want elided", got)
	}
	if _, err := f.d.Exec("DROP TABLE storecone"); err != nil {
		t.Fatal(err)
	}
	if a, err = LoadAbsences(f.d); err != nil {
		t.Fatal(err)
	}
	if a.Count(Elided) != 0 {
		t.Errorf("after losing the cone %d phantoms are elided", a.Count(Elided))
	}
}
