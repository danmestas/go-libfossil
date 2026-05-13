//go:build !js

package ncruces

import (
	"fmt"
	"strings"

	"github.com/danmestas/libfossil/db"
	_ "github.com/ncruces/go-sqlite3/driver"
)

func init() {
	db.Register(db.DriverConfig{
		Name:     "sqlite3",
		BuildDSN: buildDSN,
	})
}

func buildDSN(path string, pragmas map[string]string) string {
	if path == "" {
		panic("ncruces.buildDSN: path must not be empty")
	}
	// _txlock=immediate forces BEGIN IMMEDIATE so concurrent writers
	// serialize at BEGIN (where busy_timeout applies) instead of racing
	// on the SHARED→RESERVED upgrade (where SQLite returns SQLITE_BUSY
	// immediately to avoid deadlock, bypassing busy_timeout entirely).
	// See https://www.sqlite.org/c3ref/busy_timeout.html
	parts := []string{"_txlock=immediate"}
	for k, v := range pragmas {
		parts = append(parts, fmt.Sprintf("_pragma=%s(%s)", k, v))
	}
	return fmt.Sprintf("file:%s?%s", path, strings.Join(parts, "&"))
}
