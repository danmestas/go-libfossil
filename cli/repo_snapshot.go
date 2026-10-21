package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// RepoSnapshotCmd commits the bytes of one file onto trunk without a
// checkout: the tip's other files carry forward, and bytes equal to the tip's
// are a no-op that prints the existing tip.
type RepoSnapshotCmd struct {
	Message string `short:"m" required:"" help:"Checkin comment"`
	Path    string `required:"" help:"File name in the repository (relative, slash-separated); the bytes come from this path on disk unless --stdin"`
	Stdin   bool   `help:"Read the bytes from stdin instead of from --path on disk"`
	User    string `help:"Checkin user (default: OS username)"`
	JSON    bool   `help:"Print {\"uuid\":...,\"changed\":...} instead of the bare uuid"`
}

// snapshotResult is the --json output of snapshot.
type snapshotResult struct {
	UUID    string `json:"uuid"`
	Changed bool   `json:"changed"`
}

func (c *RepoSnapshotCmd) Run(g *Globals) (err error) {
	if err := validateRepoPath(c.Path); err != nil {
		return err
	}
	data, err := c.readContent()
	if err != nil {
		return err
	}
	user := c.User
	if user == "" {
		user = currentUser()
	}

	r, err := g.OpenRepo()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := r.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	var res snapshotResult
	err = r.Inner().WithBoundTx(func(bound *repo.Repo) error {
		var err error
		res, err = snapshotLocked(r, bound, c.Path, data, c.Message, user)
		return err
	})
	if err != nil {
		return err
	}
	if c.JSON {
		return writeJSON(res)
	}
	fmt.Println(res.UUID)
	return nil
}

func (c *RepoSnapshotCmd) readContent() ([]byte, error) {
	if c.Stdin {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(c.Path)
}

// snapshotLocked reads the trunk tip and commits name=data on it, inside the
// write transaction bound holds. Holding the write lock from the read on
// means no other process commits in between: the commit never builds on a
// stale tip, and a reader never sees an artifact half rewritten, as a
// concurrent check-in re-deltas the manifest before it. r reads through its
// own connection, which sees everything committed and, as no one else can
// commit, nothing newer.
func snapshotLocked(r *libfossil.Repo, bound *repo.Repo, name string, data []byte, msg, user string) (snapshotResult, error) {
	var checkins int64
	if err := bound.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&checkins); err != nil {
		return snapshotResult{}, err
	}

	parent, err := r.CurrentTrunk()
	if errors.Is(err, libfossil.ErrVersionNotFound) && checkins != 0 {
		// Check-ins exist but none on the main branch: a root here would
		// start a second, unrelated history.
		branch, berr := repo.MainBranch(bound.DB())
		if berr != nil {
			return snapshotResult{}, berr
		}
		return snapshotResult{}, fmt.Errorf(
			"snapshot: the repository has check-ins but none on its main branch %q", branch)
	}
	if errors.Is(err, libfossil.ErrVersionNotFound) {
		parent = 0 // an empty repository: this is the root check-in
	} else if err != nil {
		return snapshotResult{}, err
	}

	perm := ""
	if parent > 0 {
		cur, found, err := manifest.LookupFile(bound, fsltype.FslID(parent), name)
		if err != nil {
			return snapshotResult{}, err
		}
		if found {
			perm = cur.Perm
			have, err := r.ReadFile(parent, name)
			if err != nil {
				return snapshotResult{}, err
			}
			if bytes.Equal(have, data) {
				uuid, err := checkinUUID(r, parent)
				return snapshotResult{UUID: uuid}, err
			}
		}
	}

	// snapshot has no deadline of its own; the sparse required-content check
	// this bounds is a dense repository's no-op.
	files, err := manifest.MergeParentFiles(context.Background(), bound, fsltype.FslID(parent),
		[]manifest.File{{Name: name, Content: data, Perm: perm}})
	if err != nil {
		return snapshotResult{}, err
	}
	_, uuid, err := manifest.Checkin(bound, manifest.CheckinOpts{
		Files:   files,
		Comment: msg,
		User:    user,
		Parent:  fsltype.FslID(parent),
	})
	if err != nil {
		return snapshotResult{}, err
	}
	return snapshotResult{UUID: uuid, Changed: true}, nil
}

func checkinUUID(r *libfossil.Repo, rid int64) (string, error) {
	var uuid string
	err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid)
	return uuid, err
}

// validateRepoPath rejects a name fossil would not accept as a new
// repository file: one that is not a simple pathname in strict UTF-8 (file.c
// file_is_simple_pathname: no leading, doubled or trailing slash, no "." or
// ".." element, no backslash), one holding a control character, or one of the
// names fossil reserves for its own files (file_is_reserved_name).
func validateRepoPath(name string) error {
	if !deck.StrictPathname(name) {
		return fmt.Errorf("--path %q: want a clean relative path such as notes/x.md", name)
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 {
			return fmt.Errorf("--path %q: a file name may not hold a control character (byte 0x%02x)", name, name[i])
		}
	}
	if deck.IsReservedName(name) {
		return fmt.Errorf("--path %q: the name is reserved for fossil's own files", name)
	}
	return nil
}
