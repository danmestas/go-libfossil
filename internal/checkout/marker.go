package checkout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/go-libfossil/db"
)

// moveMarker moves the SQLite database, not working files in Env.Storage.
// Closing after a complete checkpoint prevents connections or WAL sidecars
// from continuing to write under the old name. Linking refuses replacement;
// interruption before unlink leaves two markers, which discovery rejects.
func (c *Checkout) moveMarker(name string) error {
	target := filepath.Join(filepath.Dir(c.dbPath), name)
	if target == c.dbPath {
		return nil
	}
	if err := checkMarkerTarget(target); err != nil {
		return err
	}
	var busy, log, checkpointed int
	if err := c.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("checkout marker checkpoint is busy")
	}
	if err := c.db.Close(); err != nil {
		return err
	}
	old := c.dbPath
	moveErr := os.Link(old, target)
	if moveErr == nil {
		if err := syncMarkerDirectory(filepath.Dir(old)); err != nil {
			moveErr = errors.Join(err, os.Remove(target))
		} else if err := os.Remove(old); err != nil {
			moveErr = errors.Join(err, os.Remove(target))
		} else {
			c.dbPath = target
			moveErr = syncMarkerDirectory(filepath.Dir(old))
		}
	}
	reopened, err := db.OpenSQL(c.dbPath, db.OpenConfig{}, nil)
	if err == nil {
		err = reopened.Ping()
	}
	if err != nil {
		if reopened != nil {
			reopened.Close()
		}
		return errors.Join(moveErr, fmt.Errorf("reopen checkout marker: %w", err))
	}
	c.db = reopened
	return moveErr
}

func syncMarkerDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func checkMarkerTarget(target string) error {
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("checkout marker already exists: %s", target)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
