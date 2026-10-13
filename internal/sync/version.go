package sync

import (
	"strconv"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The release this library announces in pragma client-version and
// server-version (§9.1): the fossil release whose protocol it speaks, and
// that release's check-in date and time.
const (
	releaseVersion = 22800
	releaseDate    = "20260315"
	releaseTime    = "120000"
)

// sha3Version is the first fossil release that accepts SHA3 artifact names.
// Content named by SHA3 is withheld from a peer older than this (§7.6).
const sha3Version = 20000

// sha1NameLen is the length of a SHA1 artifact name; any longer name is SHA3.
const sha1NameLen = 40

// versionPragma is the pragma that announces this library's release: name is
// "client-version" or "server-version".
func versionPragma(name string) *xfer.PragmaCard {
	return &xfer.PragmaCard{
		Name:   name,
		Values: []string{strconv.Itoa(releaseVersion), releaseDate, releaseTime},
	}
}

// peerVersion reads the VERSION of a client-version or server-version pragma
// as fossil's atoi does: leading digits, and 0 when there are none.
func peerVersion(c *xfer.PragmaCard) int {
	if len(c.Values) == 0 {
		panic("sync.peerVersion: pragma has no VERSION")
	}
	return leadingInt(c.Values[0])
}

// privateIGotDate is the first peer check-in date that is told, with igot
// HASH 1, of private content it asked for outside a private sync (§8.6). An
// older peer treats a private igot it may not sync as an error, so it is told
// nothing (fossil's send_file, src/xfer.c).
const privateIGotDate = 20200413

// peerDate reads the DATE of a client-version or server-version pragma as
// fossil's page_xfer and client_sync do: only when the pragma carries both
// DATE and TIME, by atoi, and 0 when it does not, which no date gate passes.
func peerDate(c *xfer.PragmaCard) int {
	if len(c.Values) < 3 {
		return 0
	}
	return leadingInt(c.Values[1])
}

// leadingInt reads s as fossil's atoi reads a pragma token: its leading
// digits, and 0 when there are none or they overflow an int.
func leadingInt(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	v, err := strconv.Atoi(s[:n])
	if err != nil {
		return 0
	}
	return v
}

// loginCookieGate reports whether a server-version names a release that
// reads a login card from the x-f-l-c cookie: 2.27 or later, built at or
// after 2025-07-27 11:05:00, as fossil's xfer_xflc_check decides (§6.5). A
// server-version without DATE and TIME does not pass.
func loginCookieGate(c *xfer.PragmaCard) bool {
	if len(c.Values) < 3 {
		return false
	}
	date, _ := strconv.Atoi(c.Values[1])
	clock, _ := strconv.Atoi(c.Values[2])
	return peerVersion(c) >= 22700 &&
		(date > 20250727 || (date == 20250727 && clock >= 110500))
}

// sha3Barred reports whether content named name is withheld from a peer of
// the given release: a SHA3 name, to a peer older than SHA3 support (§7.6).
func sha3Barred(name string, version int) bool {
	return len(name) > sha1NameLen && version < sha3Version
}

// sha3Refusal is the error card that takes the place of SHA3 content withheld
// from an older peer, worded as fossil's xfer_cannot_send_sha3_error words it.
func sha3Refusal() *xfer.ErrorCard {
	return &xfer.ErrorCard{Message: "Fossil version 2.0 or later required."}
}
