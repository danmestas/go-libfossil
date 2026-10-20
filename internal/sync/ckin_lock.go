package sync

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// DefaultCkinLockTimeout is the default duration after which a ci-lock expires.
const DefaultCkinLockTimeout = 60 * time.Second

// lockTimeout is the repository's lock-timeout setting in seconds, read as
// fossil's db_get_int("lock-timeout", 60) reads it (§9.4): absent gives 60,
// and otherwise SQLite's integer conversion applies, so a value that is not
// a number reads as 0. A timeout of 0 or less makes every lock stale at the
// next ci-lock, which is what fossil does with such a setting.
func lockTimeout(d *db.DB) time.Duration {
	var n int64
	err := d.QueryRow("SELECT CAST(value AS INTEGER) FROM config" +
		" WHERE name='lock-timeout'").Scan(&n)
	if err != nil {
		return DefaultCkinLockTimeout
	}
	return time.Duration(n) * time.Second
}

// forbidDeltaManifests reads the forbid-delta-manifests setting as fossil
// 2.29's db_get_boolean("forbid-delta-manifests", 1) does (§9.4): absent or
// NULL is on, and a value that is neither true nor false keeps that default.
// fossil 2.28's server reads it with a default of off, so it sends no
// avoid-delta-manifests unless the setting is on; this follows the draft.
func forbidDeltaManifests(d *db.DB) bool {
	if d == nil {
		panic("sync.forbidDeltaManifests: d must not be nil")
	}
	var value sql.NullString
	err := d.QueryRow("SELECT value FROM config" +
		" WHERE name='forbid-delta-manifests'").Scan(&value)
	if err != nil || !value.Valid {
		return true
	}
	return repo.SettingBoolean(value.String, true)
}

// CkinLockFail reports that another client holds the check-in lock.
type CkinLockFail struct {
	HeldBy string
	Since  time.Time
}

// ckinLockPrefix names a check-in lock's config row, as fossil's ci-lock
// does: ci-lock-HASH, holding JSON {"login":...,"clientid":...}, with the
// time the lock was taken in the mtime column (§9.4). A repository served by
// both fossil and this server so has one lock table.
const ckinLockPrefix = "ci-lock-"

func configKey(parentUUID string) string {
	return ckinLockPrefix + parentUUID
}

// releaseCkinLocks drops the check-in locks clientID holds, and any lock
// whose record is not JSON, as fossil's pragma ci-unlock does (§9.4).
func releaseCkinLocks(d *db.DB, clientID string) {
	d.Exec("DELETE FROM config WHERE name GLOB 'ci-lock-*'"+
		" AND (NOT json_valid(value) OR json_extract(value, '$.clientid')=?)", clientID)
}

// processCkinLock handles a ci-lock pragma request, as fossil's page_xfer
// does: stale locks go first; a live lock on parentUUID is kept as it is,
// with no refresh, and reported unless clientID holds it; with none, a lock
// is written for user and clientID, unacknowledged (§9.4). An empty user is
// nobody, whose login fossil stores as JSON null.
func processCkinLock(d *db.DB, parentUUID, clientID, user string,
	timeout time.Duration) *CkinLockFail {
	if parentUUID == "" {
		panic("sync.processCkinLock: empty parentUUID")
	}
	expireStaleLocks(d, timeout)

	var login, holder string
	var since int64
	// mtime is cast: the column is declared DATETIME, which some drivers
	// would otherwise hand back as a time.
	err := d.QueryRow("SELECT coalesce(json_extract(value, '$.login'), ''),"+
		" coalesce(json_extract(value, '$.clientid'), ''), CAST(mtime AS INTEGER)"+
		" FROM config"+
		" WHERE name=? AND json_valid(value)", configKey(parentUUID)).
		Scan(&login, &holder, &since)
	if err == nil {
		if holder != clientID {
			return &CkinLockFail{HeldBy: login, Since: time.Unix(since, 0)}
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		// A lock that cannot be read is not overwritten: the pragma has no
		// error reply, and taking the lock could clobber another client's.
		return nil
	}
	d.Exec("REPLACE INTO config(name, value, mtime)"+
		" VALUES(?, json_object('login', nullif(?, ''), 'clientid', ?), ?)",
		configKey(parentUUID), user, clientID, time.Now().Unix())
	return nil
}

// expireStaleLocks removes the ci-lock rows at least timeout old, or whose
// check-in is no longer a leaf. As in fossil, only rows holding JSON are
// considered; ci-unlock is what clears the others.
func expireStaleLocks(d *db.DB, timeout time.Duration) {
	cutoff := time.Now().Unix() - int64(timeout.Seconds())

	// Locks written under the old libfossil-ci-lock- key match neither this
	// sweep nor ci-unlock, so nothing else would ever clear them.
	d.Exec("DELETE FROM config WHERE name GLOB 'libfossil-ci-lock-*'")

	d.Exec("DELETE FROM config WHERE name GLOB 'ci-lock-*' AND json_valid(value)"+
		" AND mtime <= ?", cutoff)

	rows, err := d.Query("SELECT name FROM config WHERE name GLOB 'ci-lock-*'" +
		" AND json_valid(value)")
	if err != nil {
		return
	}
	defer rows.Close()

	var toDelete []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			continue
		}
		uuid := strings.TrimPrefix(name, ckinLockPrefix)
		var rid int64
		if d.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid) != nil {
			toDelete = append(toDelete, name)
			continue
		}
		var dummy int
		if d.QueryRow("SELECT 1 FROM leaf WHERE rid=?", rid).Scan(&dummy) != nil {
			toDelete = append(toDelete, name)
		}
	}
	for _, name := range toDelete {
		d.Exec("DELETE FROM config WHERE name=?", name)
	}
}
