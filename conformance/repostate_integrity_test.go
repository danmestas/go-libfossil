package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/simio"
)

// Section 4.4: an artifact whose bytes do not hash to the name it is stored
// under is an integrity failure, not an opaque artifact, and is rejected. No
// store path writes such a row, since each names an artifact by hashing it,
// so the case renames a stored check-in by SQL.

func integrityAPICases() []rsAPICase {
	return []rsAPICase{{
		Case: Case{ID: "repostate.4.4.misnamed-rejected", Req: "RS-4.4-3"},
		run:  misnamedRejected,
	}}
}

// misnamed is c1 and c2, with c2 then stored under n, a name its bytes do
// not hash to. Rejecting n leaves the rows of c1 alone.
var (
	misnamedTables = []string{"event", "leaf", "plink"}
	misnamedWant   = rsState{
		"event": {"c1 ci @1"},
		"leaf":  {"c1"},
		"plink": nil,
	}
)

// misnamedScript is c1 and c2, the SQL that renames c2 to n, and the
// labels with n among them.
func misnamedScript() (rsScript, string) {
	script := rsBuild(chain()[:2])
	name := hash.SHA3([]byte("misnamed c2"))
	script.labels[name] = "n"
	rename := fmt.Sprintf("UPDATE blob SET uuid = '%s' WHERE uuid = '%s'",
		name, script.hashes["c2"])
	return script, rename
}

func misnamedRejected(e *rsEnv) error {
	script, rename := misnamedScript()
	if e.ref != nil {
		if err := fossilMisnamed(e, script, rename); err != nil {
			return err
		}
	}
	return goMisnamed(e, script, rename)
}

// goMisnamed stores the script, renames c2, and derives the tables twice:
// by a crosslink sweep, then by verify.Rebuild, whose report must name the
// renamed artifact a hash mismatch rather than a damaged or unparsable one.
func goMisnamed(e *rsEnv, script rsScript, rename string) error {
	r, err := repo.Create(filepath.Join(e.t.TempDir(), "go.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		return err
	}
	defer r.Close()
	for _, a := range script.arts {
		if _, _, err := blob.Store(r.DB(), a.data); err != nil {
			return err
		}
	}
	if _, err := r.DB().Exec(rename); err != nil {
		return err
	}
	if _, err := manifest.Crosslink(r); err != nil {
		return expectf("Crosslink with a misnamed artifact: %v; want n rejected, c1 linked", err)
	}
	if err := sameMisnamedRows(r.Path(), script, "Crosslink"); err != nil {
		return err
	}
	report, err := verify.Rebuild(r)
	if err != nil {
		return expectf("verify.Rebuild with a misnamed artifact: %v", err)
	}
	if !reportsMismatch(report, script) {
		return expectf("verify.Rebuild issues %+v: none is a hash mismatch on n", report.Issues)
	}
	return sameMisnamedRows(r.Path(), script, "verify.Rebuild")
}

func reportsMismatch(report *verify.Report, script rsScript) bool {
	for _, issue := range report.Issues {
		if issue.Kind == verify.IssueHashMismatch && script.labels[issue.UUID] == "n" {
			return true
		}
	}
	return false
}

func sameMisnamedRows(path string, script rsScript, name string) error {
	got, err := readState(path, script.labels, misnamedTables, nil)
	if err != nil {
		return err
	}
	if d := diffState(misnamedWant, got, "want", name); d != "" {
		return expectf("%s", d)
	}
	return nil
}

// fossilMisnamed puts the script into fossil(1) without crosslinking it,
// renames c2 and rebuilds. fossil's test-integrity reports the wrong hash,
// but its rebuild links n as though it were c2: the rows it derives are
// not want. The case follows the draft and fails once fossil agrees.
func fossilMisnamed(e *rsEnv, script rsScript, rename string) error {
	f, err := e.ref.newRepo()
	if err != nil {
		return err
	}
	for i, a := range script.arts {
		file := filepath.Join(filepath.Dir(f.path), fmt.Sprintf("artifact-%02d", i))
		if err := os.WriteFile(file, a.data, 0o644); err != nil {
			return err
		}
		if _, err := e.ref.run(f.checkout, "test-content-put", file); err != nil {
			return err
		}
	}
	if _, err := e.ref.run(f.checkout, "sql", rename); err != nil {
		return err
	}
	// test-integrity exits non-zero when it finds a problem, which is the
	// finding being checked, so the error is not a failure here.
	out, _ := e.ref.run(f.checkout, "test-integrity")
	if !strings.Contains(out, "wrong hash") {
		return expectf("fossil test-integrity: %q; the case expects a wrong-hash report", out)
	}
	if _, err := e.ref.run(f.checkout, "rebuild"); err != nil {
		return err
	}
	got, err := readState(f.path, script.labels, misnamedTables, nil)
	if err != nil {
		return err
	}
	if diffState(misnamedWant, got, "want", "fossil rebuild") == "" {
		return expectf("fossil rebuild now rejects the misnamed artifact; remove the note " +
			"in fossilMisnamed")
	}
	return nil
}
