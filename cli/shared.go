package cli

import (
	"database/sql"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	libfossil "github.com/danmestas/libfossil"
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

// repoFromCheckout reads the repository path from a .fslckout database.
func repoFromCheckout(ckoutPath string) (string, error) {
	db, err := sql.Open("sqlite", ckoutPath+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var repoPath string
	err = db.QueryRow("SELECT value FROM vvar WHERE name='repository'").Scan(&repoPath)
	if err != nil {
		return "", fmt.Errorf("checkout %s: no repository path found", ckoutPath)
	}
	return repoPath, nil
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
	return sql.Open("sqlite", ckoutPath)
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
