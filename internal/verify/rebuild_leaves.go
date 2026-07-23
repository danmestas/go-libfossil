package verify

import (
	"fmt"

	"github.com/danmestas/go-libfossil/db"
)

// rebuildLeaves populates the leaf table from event/plink data.
// A leaf is a checkin that is not a parent of any other checkin.
func rebuildLeaves(tx *db.Tx) error {
	if tx == nil {
		panic("rebuildLeaves: nil *db.Tx")
	}

	_, err := tx.Exec(`
		INSERT INTO leaf(rid)
		SELECT e.objid FROM event e
		WHERE e.type = 'ci'
		AND NOT EXISTS (SELECT 1 FROM plink WHERE pid = e.objid)
	`)
	if err != nil {
		return fmt.Errorf("rebuildLeaves: %w", err)
	}
	return nil
}

// rebuildBookkeeping populates unclustered and unsent tables.
// Every non-phantom blob is marked as unclustered and unsent to
// ensure sync will re-evaluate what needs to be pushed.
func rebuildBookkeeping(tx *db.Tx) error {
	if tx == nil {
		panic("rebuildBookkeeping: nil *db.Tx")
	}

	if _, err := tx.Exec("INSERT INTO unclustered(rid) SELECT rid FROM blob WHERE size >= 0"); err != nil {
		return fmt.Errorf("rebuildBookkeeping unclustered: %w", err)
	}
	if _, err := tx.Exec("INSERT INTO unsent(rid) SELECT rid FROM blob WHERE size >= 0"); err != nil {
		return fmt.Errorf("rebuildBookkeeping unsent: %w", err)
	}
	return nil
}
