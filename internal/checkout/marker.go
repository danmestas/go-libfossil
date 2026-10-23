package checkout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/go-libfossil/db"
)

// markerCrash is nil outside tests, which set it to simulate a process
// death after a named step of a marker transition.
var markerCrash func(step string)

func markerStep(step string) {
	if markerCrash != nil {
		markerCrash(step)
	}
}

// markerMoveVVar records the name a marker transition is moving to. It is
// written before linking, so two names for one database always say which
// one the transition keeps.
const markerMoveVVar = "libfossil-marker-move"

// moveMarker moves the SQLite database, not working files in Env.Storage.
// Closing after a complete checkpoint prevents connections or WAL sidecars
// from continuing to write under the old name. Linking refuses replacement;
// interruption before unlink leaves two names for one database, which Open
// completes to the recorded target (recoverMarkerPair). Open completing it
// concurrently removes the old name first, which counts as done here.
func (c *Checkout) moveMarker(name string) error {
	target := filepath.Join(filepath.Dir(c.dbPath), name)
	if target == c.dbPath {
		return nil
	}
	if err := checkMarkerTarget(target); err != nil {
		return err
	}
	if err := setVVar(c.db, markerMoveVVar, name); err != nil {
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
	markerStep("closed")
	moveErr := c.linkMarker(target)
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

func (c *Checkout) linkMarker(target string) error {
	old := c.dbPath
	if err := os.Link(old, target); err != nil {
		return err
	}
	markerStep("linked")
	if err := syncMarkerDirectory(filepath.Dir(old)); err != nil {
		return errors.Join(err, os.Remove(target))
	}
	if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
		return errors.Join(err, os.Remove(target))
	}
	c.dbPath = target
	err := syncMarkerDirectory(filepath.Dir(old))
	markerStep("unlinked")
	return err
}

func syncMarkerDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func checkMarkerTarget(target string) error {
	// Opening SQLite can overwrite or remove sidecars even when its main file
	// does not exist. Lstat also refuses dangling links and filesystem aliases.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		name := target + suffix
		if _, err := os.Lstat(name); err == nil {
			return fmt.Errorf("checkout marker already exists: %s", name)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// markerPairError is one checkout database under both the native and the
// private name: what a crash between moveMarker's link and unlink leaves.
type markerPairError struct{ native, private string }

func (e *markerPairError) Error() string {
	return fmt.Sprintf("multiple checkout markers in %s: %s and %s (one database)",
		filepath.Dir(e.native), e.native, e.private)
}

// linkedMarkerPair reports a and b as a pair when they are the native and the
// private name of one file. Distinct files under both names stay ambiguous.
func linkedMarkerPair(a, b string) *markerPairError {
	native, private := a, b
	if filepath.Base(native) == sparseDBName {
		native, private = b, a
	}
	if filepath.Base(private) != sparseDBName || filepath.Base(native) == sparseDBName {
		return nil
	}
	ni, nerr := os.Lstat(native)
	pi, perr := os.Lstat(private)
	if nerr != nil || perr != nil || !ni.Mode().IsRegular() || !sameFile(ni, pi) {
		return nil
	}
	return &markerPairError{native: native, private: private}
}

// recoverMarkerPair completes the interrupted move to the name moveMarker
// recorded before linking: the same survivor a still-running transition
// keeps, so the two never remove both names. Every transition links while
// the record is dense or already under its legacy native name, so the
// survivor is never a new sparse record under the name stock fossil opens.
func recoverMarkerPair(p *markerPairError) error {
	if filepath.Dir(p.native) != filepath.Dir(p.private) {
		panic("checkout.recoverMarkerPair: names in different directories")
	}
	ckdb, err := db.OpenSQL(p.private, db.OpenConfig{}, nil)
	if err != nil {
		return err
	}
	target, err := getVVar(ckdb, markerMoveVVar)
	sparse, coneErr := readCone(ckdb)
	if err := errors.Join(err, coneErr, ckdb.Close()); err != nil {
		return fmt.Errorf("recover checkout marker: %w", err)
	}
	drop := p.private
	switch target {
	case filepath.Base(p.private):
		drop = p.native
	case filepath.Base(p.native):
	default:
		if sparse != nil {
			drop = p.native
		}
	}
	if err := os.Remove(drop); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("recover checkout marker: %w", err)
	}
	if err := syncMarkerDirectory(filepath.Dir(drop)); err != nil {
		return fmt.Errorf("recover checkout marker: %w", err)
	}
	if err := checkMarkerTarget(drop); err != nil {
		return fmt.Errorf("recover checkout marker: stale sidecar: %w", err)
	}
	return nil
}
