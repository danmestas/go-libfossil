package conformance

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	gofossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/checkout"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// The requirements that are about an API rather than the derived tables:
// name resolution, leaves, branches and tagging. Each case loads its script
// into go-libfossil (one sweep) and into fossil(1) (one artifact at a
// time), then asks both the same question. fossil's answer must be the
// case's, so the expectation is checked on every run.

// rsAPICase asks an API a question about a scripted repository.
type rsAPICase struct {
	Case
	arts []rsArt
	// run returns nil when go-libfossil gives the case's answer.
	run func(e *rsEnv) error
}

// rsEnv is a case's script loaded into both implementations.
type rsEnv struct {
	t      *testing.T
	script rsScript
	goPath string
	ref    *fossilReference // nil without fossil(1)
	fossil fossilRepo
}

func TestRepoStateAPI(t *testing.T) {
	ref := newFossilReference(t)
	for _, c := range rsAPICases() {
		check(t, c.Case, func() error {
			e, err := newEnv(t, ref, c.arts)
			if err != nil {
				return err
			}
			return c.run(e)
		})
	}
}

func newEnv(t *testing.T, ref *fossilReference, arts []rsArt) (*rsEnv, error) {
	e := &rsEnv{t: t, script: rsBuild(arts), ref: ref}
	path, err := goLoad(t, e.script, loadSweep, nil)
	if err != nil {
		return nil, err
	}
	e.goPath = path
	if ref != nil {
		if e.fossil, err = ref.loadScript(e.script, nil); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// fossilRun runs fossil(1) in the case's checkout.
func (e *rsEnv) fossilRun(args ...string) (string, error) {
	return e.ref.run(e.fossil.checkout, args...)
}

// open opens the go-libfossil repository; the test closes it.
func (e *rsEnv) open() (*gofossil.Repo, error) {
	r, err := gofossil.Open(e.goPath)
	if err != nil {
		return nil, err
	}
	e.t.Cleanup(func() { r.Close() })
	return r, nil
}

// goRid is the rid go-libfossil stored label under.
func (e *rsEnv) goRid(r *repo.Repo, label string) (libfossil.FslID, error) {
	var rid int64
	err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid = ?", e.script.hashes[label]).Scan(&rid)
	return libfossil.FslID(rid), err
}

// labelOfRid names a go-libfossil rid by its label.
func (e *rsEnv) labelOfRid(r *repo.Repo, rid int64) string {
	var uuid string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid = ?", rid).Scan(&uuid); err != nil {
		return fmt.Sprintf("rid %d (%v)", rid, err)
	}
	return labelOf(uuid, e.script.labels)
}

// labelOfPrefix names the artifact whose hash starts with prefix.
func (e *rsEnv) labelOfPrefix(prefix string) string {
	for uuid, label := range e.script.labels {
		if strings.HasPrefix(uuid, prefix) {
			return label
		}
	}
	return prefix
}

// resolves checks that name resolves to the artifact labelled want, or, when
// want is empty, that it does not resolve. In name, "{rid:L}" stands for
// the rid each repository stored L under.
func resolves(id, req string, arts []rsArt, name, want string) rsAPICase {
	return rsAPICase{Case: Case{ID: id, Req: req}, arts: arts, run: func(e *rsEnv) error {
		if e.ref != nil {
			got, err := e.fossilResolve(name)
			if err != nil {
				return err
			}
			if got != want {
				return expectf("fossil(1) resolves %q to %q; the case expects %q", name, got, want)
			}
		}
		got, err := e.goResolve(name)
		if err != nil {
			return err
		}
		if got != want {
			return expectf("ResolveVersion(%q) = %q, want %q", name, got, want)
		}
		return nil
	}}
}

var ridPlaceholder = regexp.MustCompile(`\{rid:([a-z0-9]+)\}`)

// fossilResolve is the label fossil's test-name-to-id gives name, or "" for
// an error.
func (e *rsEnv) fossilResolve(name string) (string, error) {
	for _, m := range ridPlaceholder.FindAllStringSubmatch(name, -1) {
		query := "SELECT rid FROM blob WHERE uuid = '" + e.script.hashes[m[1]] + "'"
		out, err := e.fossilRun("sql", query)
		if err != nil {
			return "", err
		}
		name = strings.ReplaceAll(name, m[0], strings.TrimSpace(out))
	}
	out, err := e.fossilRun("test-name-to-id", name)
	if err != nil {
		return "", err
	}
	_, result, ok := strings.Cut(strings.TrimSpace(out), " -> ")
	if !ok {
		return "", fmt.Errorf("test-name-to-id: unrecognized output %q", out)
	}
	if strings.HasPrefix(result, "ERROR") {
		return "", nil
	}
	return labelOf(result, e.script.labels), nil
}

// goResolve is the label ResolveVersion gives name, or "" for an error
// that says the name is unknown or ambiguous.
func (e *rsEnv) goResolve(name string) (string, error) {
	r, err := e.open()
	if err != nil {
		return "", err
	}
	for _, m := range ridPlaceholder.FindAllStringSubmatch(name, -1) {
		rid, err := e.goRid(r.Inner(), m[1])
		if err != nil {
			return "", err
		}
		name = strings.ReplaceAll(name, m[0], fmt.Sprint(rid))
	}
	rid, err := r.ResolveVersion(name)
	if errors.Is(err, gofossil.ErrVersionNotFound) || errors.Is(err, gofossil.ErrAmbiguousVersion) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return e.labelOfRid(r.Inner(), rid), nil
}

// leafLine is one line of `fossil leaves`: the short hash and the tags.
var leafLine = regexp.MustCompile(`\[([0-9a-f]{10})\].*tags: ([^)]*)\)`)

// openLeaves checks the open leaves of branch: fossil's `leaves`, which
// lists open leaves, filtered to that branch, and checkout.BranchLeaves.
func openLeaves(id, req string, arts []rsArt, branch string, want ...string) rsAPICase {
	sort.Strings(want)
	return rsAPICase{Case: Case{ID: id, Req: req}, arts: arts, run: func(e *rsEnv) error {
		if e.ref != nil {
			got, err := e.fossilLeaves(branch)
			if err != nil {
				return err
			}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				return expectf("fossil leaves on %s: %v; the case expects %v", branch, got, want)
			}
		}
		got, err := e.goLeaves(branch)
		if err != nil {
			return err
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			return expectf("BranchLeaves(%q) = %v, want %v", branch, got, want)
		}
		return nil
	}}
}

func (e *rsEnv) fossilLeaves(branch string) ([]string, error) {
	out, err := e.fossilRun("leaves")
	if err != nil {
		return nil, err
	}
	var labels []string
	for _, m := range leafLine.FindAllStringSubmatch(out, -1) {
		for _, tag := range strings.Split(m[2], ", ") {
			if tag == branch {
				labels = append(labels, e.labelOfPrefix(m[1]))
			}
		}
	}
	sort.Strings(labels)
	return labels, nil
}

func (e *rsEnv) goLeaves(branch string) ([]string, error) {
	r, err := e.open()
	if err != nil {
		return nil, err
	}
	rids, err := checkout.BranchLeaves(r.Inner(), branch)
	if err != nil {
		return nil, err
	}
	labels := []string{}
	for _, rid := range rids {
		labels = append(labels, e.labelOfRid(r.Inner(), int64(rid)))
	}
	sort.Strings(labels)
	return labels, nil
}

// wouldFork checks the ambiguity test for a checkout of at: fossil's
// test-leaf-ambiguity and (*Checkout).WouldFork.
func wouldFork(id, req string, arts []rsArt, at string, want bool) rsAPICase {
	return rsAPICase{Case: Case{ID: id, Req: req}, arts: arts, run: func(e *rsEnv) error {
		if e.ref != nil {
			out, err := e.fossilRun("test-leaf-ambiguity", e.script.hashes[at])
			if err != nil {
				return err
			}
			if got := !strings.HasSuffix(strings.TrimSpace(out), " ok"); got != want {
				return expectf("fossil test-leaf-ambiguity %s: %q; the case expects ambiguous=%v",
					at, strings.TrimSpace(out), want)
			}
		}
		got, err := e.goWouldFork(at)
		if err != nil {
			return err
		}
		if got != want {
			return expectf("WouldFork on %s = %v, want %v", at, got, want)
		}
		return nil
	}}
}

func (e *rsEnv) goWouldFork(at string) (bool, error) {
	r, err := e.open()
	if err != nil {
		return false, err
	}
	rid, err := e.goRid(r.Inner(), at)
	if err != nil {
		return false, err
	}
	co, err := checkout.Create(r.Inner(), filepath.Join(e.t.TempDir(), "co"), checkout.CreateOpts{})
	if err != nil {
		return false, err
	}
	defer co.Close()
	if err := co.Extract(rid, checkout.ExtractOpts{Force: true}); err != nil {
		return false, err
	}
	return co.WouldFork()
}
