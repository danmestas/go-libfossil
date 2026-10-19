package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// snapshotAttempts bounds the retries when another process commits between
// reading the trunk tip and writing on it. Each lost round means another
// writer made progress, so a handful covers any realistic contention.
const snapshotAttempts = 20

// errTrunkMoved is a snapshot's own commit finding the repository changed
// since it read the trunk tip.
var errTrunkMoved = errors.New("trunk moved since its tip was read")

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

	for range snapshotAttempts {
		res, err := snapshotOnce(r, c.Path, data, c.Message, user)
		if errors.Is(err, errTrunkMoved) {
			continue
		}
		if err != nil {
			return err
		}
		if c.JSON {
			return writeJSON(res)
		}
		fmt.Println(res.UUID)
		return nil
	}
	return fmt.Errorf("snapshot: trunk kept moving after %d attempts", snapshotAttempts)
}

func (c *RepoSnapshotCmd) readContent() ([]byte, error) {
	if c.Stdin {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(c.Path)
}

// snapshotOnce reads the trunk tip and commits name=data on it. The commit
// re-checks, in its own write transaction, that no check-in arrived since the
// read; otherwise it returns errTrunkMoved and the caller starts over, so a
// commit never builds on a stale tip and drops another writer's files.
func snapshotOnce(r *libfossil.Repo, name string, data []byte, msg, user string) (snapshotResult, error) {
	h := r.Inner()
	stamp, err := checkinStamp(r.DB())
	if err != nil {
		return snapshotResult{}, err
	}

	parent, err := r.CurrentTrunk()
	if errors.Is(err, libfossil.ErrVersionNotFound) {
		parent = 0 // an empty repository: this is the root check-in
	} else if err != nil {
		return snapshotResult{}, err
	}

	perm := ""
	if parent > 0 {
		cur, found, err := manifest.LookupFile(h, fsltype.FslID(parent), name)
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

	files, err := manifest.MergeParentFiles(h, fsltype.FslID(parent),
		[]manifest.File{{Name: name, Content: data, Perm: perm}})
	if err != nil {
		return snapshotResult{}, err
	}
	_, uuid, err := manifest.Checkin(h, manifest.CheckinOpts{
		Files:   files,
		Comment: msg,
		User:    user,
		Parent:  fsltype.FslID(parent),
		Precheck: func(q db.Querier) error {
			now, err := checkinStamp(q)
			if err != nil {
				return err
			}
			if now != stamp {
				return errTrunkMoved
			}
			return nil
		},
	})
	if err != nil {
		return snapshotResult{}, err
	}
	return snapshotResult{UUID: uuid, Changed: true}, nil
}

// checkinStamp identifies the repository's check-ins as of now: the newest
// check-in's rid, which any new check-in raises.
func checkinStamp(q db.Querier) (int64, error) {
	var stamp int64
	err := q.QueryRow("SELECT coalesce(max(objid), 0) FROM event WHERE type='ci'").Scan(&stamp)
	return stamp, err
}

func checkinUUID(r *libfossil.Repo, rid int64) (string, error) {
	var uuid string
	err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid)
	return uuid, err
}

// validateRepoPath rejects a name that is not a clean relative path, as
// fossil does for a file card: no leading or doubled slash, no "." or ".."
// element.
func validateRepoPath(name string) error {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name ||
		name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return fmt.Errorf("--path %q: want a clean relative path such as notes/x.md", name)
	}
	return nil
}
