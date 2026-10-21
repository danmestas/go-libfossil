package checkout

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

type markerCrashed struct{ step string }

// crashMarkerAt makes the next marker transition die after step: the panic
// unwinds past every cleanup the transition would otherwise run, which is
// what a process death leaves on disk.
func crashMarkerAt(t *testing.T, step string) {
	t.Helper()
	markerCrash = func(at string) {
		if at == step {
			panic(markerCrashed{step})
		}
	}
	t.Cleanup(func() { markerCrash = nil })
}

func setConeCrashes(co *Checkout, next *cone.Cone) bool {
	return crashes(func() { co.SetCone(next) })
}

func crashes(run func()) (crashed bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(markerCrashed); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	run()
	return false
}

// Open's migration of a legacy sparse record off the native name is the
// third transition; a crash at any step reopens as the migrated checkout.
func TestLegacyMigrationCrashRecovers(t *testing.T) {
	for _, step := range []string{"closed", "linked", "unlinked"} {
		t.Run(step, func(t *testing.T) {
			f := newSparseRepo(t, false)
			c, err := Create(f.repo, t.TempDir(), CreateOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Extract(f.c1, ExtractOpts{Force: true}); err != nil {
				t.Fatal(err)
			}
			sparse := sparseCone(t)
			if err := writeCone(c.db, sparse); err != nil {
				t.Fatal(err)
			}
			for name := range sparseFiles {
				if !sparse.Covers(name) {
					if err := os.Remove(filepath.Join(c.Dir(), filepath.FromSlash(name))); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			crashMarkerAt(t, step)
			if !crashes(func() { Open(f.repo, c.Dir(), OpenOpts{}) }) {
				t.Fatalf("migration never reached step %q", step)
			}
			markerCrash = nil
			reopened, err := Open(f.repo, c.Dir(), OpenOpts{})
			if err != nil {
				t.Fatalf("open after a migration crash at %q: %v", step, err)
			}
			defer reopened.Close()
			assertCheckoutMarker(t, c.Dir(), sparseDBName)
			checkCrashState(t, reopened, reopened.Cone(), sparse, sparse)
		})
	}
}

// A crash at any step of a marker transition reopens as the transition's
// old or new state, never both and never neither; stock fossil either
// refuses the directory or reads the dense record it holds.
func TestMarkerTransitionCrashRecovers(t *testing.T) {
	bin := testutil.FossilBinary()
	if bin != "" {
		testutil.IsolateFossilHome(t)
	}
	for _, narrowing := range []bool{true, false} {
		for _, step := range []string{"closed", "linked", "unlinked"} {
			name := "widen/" + step
			if narrowing {
				name = "narrow/" + step
			}
			t.Run(name, func(t *testing.T) { testMarkerCrash(t, bin, narrowing, step) })
		}
	}
}

func testMarkerCrash(t *testing.T, bin string, narrowing bool, step string) {
	f := newSparseRepo(t, false)
	var initial, next *cone.Cone
	if narrowing {
		next = sparseCone(t)
	} else {
		initial = sparseCone(t)
	}
	dir := t.TempDir()
	co, err := Create(f.repo, dir, CreateOpts{Cone: initial})
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Extract(f.c1, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	crashMarkerAt(t, step)
	if !setConeCrashes(co, next) {
		t.Fatalf("SetCone never reached step %q", step)
	}
	markerCrash = nil
	// fossil cannot read a repository go-libfossil holds open.
	path := f.repo.Path()
	f.repo.Close()
	if bin != "" {
		checkFossilReadsCrash(t, bin, dir)
	}
	r, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		reopened, err := Open(r, dir, OpenOpts{})
		if err != nil {
			t.Fatalf("open after a crash at %q (round %d): %v", step, round, err)
		}
		got := reopened.Cone()
		checkCrashState(t, reopened, got, initial, next)
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if bin != "" {
		checkFossilReadsCrash(t, bin, dir)
	}
}

func checkCrashState(t *testing.T, co *Checkout, got, initial, next *cone.Cone) {
	t.Helper()
	digest := func(c *cone.Cone) string {
		if c == nil {
			return ""
		}
		return c.Digest()
	}
	if digest(got) != digest(initial) && digest(got) != digest(next) {
		t.Fatalf("reopened cone %v is neither the old nor the new one", got)
	}
	marker, err := checkoutMarker(simio.OSStorage{}, co.Dir())
	if err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if got != nil && filepath.Base(marker) != sparseDBName {
		t.Fatalf("sparse record reopened under native marker %s", marker)
	}
	want := sparseCovered
	if got == nil {
		want = nil
		for name := range sparseFiles {
			want = append(want, name)
		}
		sort.Strings(want)
	}
	if disk := onDisk(t, co); !slices.Equal(disk, want) {
		t.Fatalf("on disk %q, want %q for cone %v", disk, want, got)
	}
}

// checkFossilReadsCrash runs stock fossil in the recovered directory: with no
// native marker it must refuse; with one it must list the dense record.
func checkFossilReadsCrash(t *testing.T, bin, dir string) {
	t.Helper()
	cmd := exec.Command(bin, "ls")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "USER=test")
	out, err := cmd.CombinedOutput()
	_, nativeErr := os.Lstat(filepath.Join(dir, PreferredDBName()))
	if nativeErr != nil {
		if err == nil || !strings.Contains(string(out), "not within an open check") {
			t.Fatalf("fossil read a directory with no native marker: %v\n%s", err, out)
		}
		return
	}
	if err != nil {
		t.Fatalf("fossil ls over the native marker: %v\n%s", err, out)
	}
	listed := strings.Fields(string(out))
	sort.Strings(listed)
	var want []string
	for name := range sparseFiles {
		want = append(want, name)
	}
	sort.Strings(want)
	if !slices.Equal(listed, want) {
		t.Fatalf("fossil lists %q under the native marker, want the dense record %q", listed, want)
	}
}

// An Open that finds the pair while another process is still between link
// and unlink completes the same move that process is making: the transition
// then finishes, and exactly the target name remains.
func TestMarkerRecoveryDuringLiveTransition(t *testing.T) {
	for _, narrowing := range []bool{true, false} {
		f := newSparseRepo(t, false)
		var initial, next *cone.Cone
		want := PreferredDBName()
		if narrowing {
			next, want = sparseCone(t), sparseDBName
		} else {
			initial = sparseCone(t)
		}
		co, err := Create(f.repo, t.TempDir(), CreateOpts{Cone: initial})
		if err != nil {
			t.Fatal(err)
		}
		if err := co.Extract(f.c1, ExtractOpts{Force: true}); err != nil {
			t.Fatal(err)
		}
		markerCrash = func(step string) {
			if step != "linked" {
				return
			}
			_, err := checkoutMarker(simio.OSStorage{}, co.Dir())
			var pair *markerPairError
			if !errors.As(err, &pair) {
				t.Fatalf("no linked pair at the linked step: %v", err)
			}
			if err := recoverMarkerPair(pair); err != nil {
				t.Fatalf("concurrent recovery: %v", err)
			}
		}
		_, err = co.SetCone(next)
		markerCrash = nil
		if err != nil {
			t.Fatalf("narrowing=%v: transition after a concurrent recovery: %v", narrowing, err)
		}
		assertCheckoutMarker(t, co.Dir(), want)
		if err := co.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
