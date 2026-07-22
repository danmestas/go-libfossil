package sync

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
)

// DefaultCkinLockTimeout is the default duration after which a ci-lock expires.
const DefaultCkinLockTimeout = 60 * time.Second

// CkinLockFail reports that another client holds the check-in lock.
type CkinLockFail struct {
	HeldBy string
	Since  time.Time
}

type ckinLockEntry struct {
	ClientID string `json:"clientid"`
	Login    string `json:"login"`
	MTime    int64  `json:"mtime"`
}

func configKey(parentUUID string) string {
	return "libfossil-ci-lock-" + parentUUID
}

// processCkinLock handles a ci-lock pragma request.
// Returns non-nil CkinLockFail if another client holds the lock.
func processCkinLock(d *db.DB, parentUUID, clientID, user string, timeout time.Duration) *CkinLockFail {
	expireStaleLocks(d, timeout)

	key := configKey(parentUUID)
	var raw string
	err := d.QueryRow("SELECT value FROM config WHERE name=?", key).Scan(&raw)
	if err == nil {
		var entry ckinLockEntry
		if json.Unmarshal([]byte(raw), &entry) == nil && entry.ClientID != clientID {
			return &CkinLockFail{
				HeldBy: entry.Login,
				Since:  time.Unix(entry.MTime, 0),
			}
		}
	}

	// Upsert lock.
	now := time.Now().Unix()
	entry := ckinLockEntry{ClientID: clientID, Login: user, MTime: now}
	val, _ := json.Marshal(entry)
	d.Exec(`REPLACE INTO config(name, value, mtime) VALUES(?, ?, ?)`, key, string(val), now)
	return nil
}

// expireStaleLocks removes ci-lock entries that are older than timeout
// or whose parent UUID is no longer in the leaf table.
func expireStaleLocks(d *db.DB, timeout time.Duration) {
	cutoff := time.Now().Unix() - int64(timeout.Seconds())

	d.Exec("DELETE FROM config WHERE name LIKE 'libfossil-ci-lock-%' AND mtime < ?", cutoff)

	rows, err := d.Query("SELECT name FROM config WHERE name LIKE 'libfossil-ci-lock-%'")
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
		uuid := strings.TrimPrefix(name, "libfossil-ci-lock-")
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
