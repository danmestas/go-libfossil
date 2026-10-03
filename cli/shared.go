package cli

import (
	"database/sql"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	libdb "github.com/danmestas/go-libfossil/db"

	libfossil "github.com/danmestas/go-libfossil"
)

// Globals holds flags shared by all CLI commands.
type Globals struct {
	Repo    string `short:"R" help:"Path to repository file" type:"path"`
	Verbose bool   `short:"v" help:"Verbose output"`
}

// OpenRepo opens a Fossil repository using the handle API.
// If Repo is empty, it searches for a .fossil file or .fslckout checkout.
func (g *Globals) OpenRepo() (*libfossil.Repo, error) {
	if g.Repo == "" {
		found, err := findRepo()
		if err != nil {
			return nil, fmt.Errorf("no repository specified (use -R <path>)")
		}
		g.Repo = found
	}
	return libfossil.Open(g.Repo)
}

// resolveRID resolves a version string to a rid.
// Accepts: empty/"tip" (most recent checkin), "trunk" (tagged trunk tip),
// named branch, UUID prefix (min 4 chars), or full UUID.
//
// This is a thin CLI-layer wrapper around Repo.ResolveVersion, which holds
// the canonical resolution logic.
func resolveRID(r *libfossil.Repo, version string) (int64, error) {
	return r.ResolveVersion(version)
}

// currentUser returns the current OS username, or "anonymous" if unavailable.
func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "anonymous"
}

// findRepo searches the current directory and its parents for a .fossil file
// or a .fslckout checkout database that points to a repo.
func findRepo() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		ckout := filepath.Join(dir, ".fslckout")
		if _, err := os.Stat(ckout); err == nil {
			return repoFromCheckout(ckout)
		}

		matches, _ := filepath.Glob(filepath.Join(dir, "*.fossil"))
		if len(matches) == 1 {
			return matches[0], nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no .fossil file found")
}

// repoFromCheckout reads the repository path from a checkout database. A
// relative path, as fossil open stores it, is relative to the checkout's
// directory. The database is opened read-write: forcing read-only clashes
// with the WAL pragma every connection sets, on a database fossil left in
// delete journal mode; the library opens it read-write as well.
func repoFromCheckout(ckoutPath string) (path string, err error) {
	db, err := libdb.OpenSQL(ckoutPath, libdb.OpenConfig{}, nil)
	if err != nil {
		return "", fmt.Errorf("checkout %s: %w", ckoutPath, err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	var repoPath string
	if err := db.QueryRow(
		"SELECT value FROM vvar WHERE name='repository'",
	).Scan(&repoPath); err != nil {
		return "", fmt.Errorf("checkout %s: read repository path: %w", ckoutPath, err)
	}
	if !filepath.IsAbs(repoPath) {
		repoPath = filepath.Join(filepath.Dir(ckoutPath), repoPath)
	}
	return repoPath, nil
}

// openWorkingCheckout opens the checkout in dir through the library, with
// the repository it belongs to: -R when given, otherwise the repository the
// checkout records. done closes both and reports the first error.
func openWorkingCheckout(
	g *Globals, dir string,
) (co *libfossil.Checkout, done func() error, err error) {
	if g == nil {
		panic("cli.openWorkingCheckout: nil globals")
	}
	if dir == "" {
		panic("cli.openWorkingCheckout: empty dir")
	}

	if g.Repo == "" {
		ckoutPath, err := checkoutDBPath(dir)
		if err != nil {
			return nil, nil, err
		}
		if g.Repo, err = repoFromCheckout(ckoutPath); err != nil {
			return nil, nil, err
		}
	}
	r, err := g.OpenRepo()
	if err != nil {
		return nil, nil, err
	}
	co, err = r.OpenCheckout(dir, libfossil.CheckoutOpenOpts{})
	if err != nil {
		if cerr := r.Close(); cerr != nil {
			return nil, nil, fmt.Errorf("%w (closing repository: %v)", err, cerr)
		}
		return nil, nil, err
	}
	done = func() error {
		coErr := co.Close()
		repoErr := r.Close()
		if coErr != nil {
			return coErr
		}
		return repoErr
	}
	return co, done, nil
}

// closeWith runs done and keeps its error when the command had none, so a
// failure to close is reported rather than lost.
func closeWith(done func() error, err *error) {
	if cerr := done(); cerr != nil && *err == nil {
		*err = cerr
	}
}

// checkoutDBPath returns the path of the checkout database in dir.
func checkoutDBPath(dir string) (string, error) {
	for _, name := range []string{".fslckout", "_FOSSIL_"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no checkout found in %s (run 'fossil repo open' first)", dir)
}

// openCheckout opens the .fslckout database in the given directory.
func openCheckout(dir string) (*sql.DB, error) {
	ckoutPath := filepath.Join(dir, ".fslckout")
	if _, err := os.Stat(ckoutPath); err != nil {
		ckoutPath = filepath.Join(dir, "_FOSSIL_")
		if _, err := os.Stat(ckoutPath); err != nil {
			return nil, fmt.Errorf("no checkout found in %s (run 'fossil repo open' first)", dir)
		}
	}
	return libdb.OpenSQL(ckoutPath, libdb.OpenConfig{}, nil)
}

// checkoutVid returns the current checkout version ID from vvar.
func checkoutVid(db *sql.DB) (int64, error) {
	var vid int64
	err := db.QueryRow("SELECT value FROM vvar WHERE name='checkout'").Scan(&vid)
	if err != nil {
		return 0, fmt.Errorf("reading checkout version: %w", err)
	}
	return vid, nil
}
