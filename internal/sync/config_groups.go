package sync

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// This file owns what a reqconfig card may read: the configuration groups
// fossil transfers, the names in each, and the capability each needs. It
// mirrors fossil 2.28's configure.c (aGroupName, aConfig,
// configure_send_group) and the reqconfig branch of xfer.c page_xfer. A
// config row outside these tables — project-code, server-code, any local
// setting — is never sent, whatever the requester's capabilities.

// Configuration group bits, fossil's CONFIGSET_* values.
const (
	cfgCSS     = 1 << iota // style sheet only
	cfgSkin                // web interface appearance
	cfgTicket              // ticket setup and the reportfmt table
	cfgProject             // project name and repository policy
	cfgShun                // the shun table
	cfgUser                // the user table, password hashes included
	cfgAddr                // the concealed table of email addresses
	cfgXfer                // transfer hook scripts
	cfgAlias               // walias:/ URL aliases
	cfgScriber             // the subscriber table
	cfgIWiki               // interwiki: prefixes
	cfgAll     = 1<<iota - 1
)

// configGroups is fossil's aGroupName, in its order: a request matches the
// first group whose name it prefixes, so the order is part of the protocol.
var configGroups = []struct {
	name string
	mask int
}{
	{"email", cfgAddr},
	{"project", cfgProject},
	{"skin", cfgSkin | cfgCSS},
	{"css", cfgCSS},
	{"shun", cfgShun},
	{"ticket", cfgTicket},
	{"user", cfgUser},
	{"xfer", cfgXfer},
	{"alias", cfgAlias},
	{"subscriber", cfgScriber},
	{"interwiki", cfgIWiki},
	{"all", cfgAll},
}

// configScalars is the scalar part of fossil 2.28's aConfig, in its order,
// which is the order the cards go out in. The "@" entries there are the
// tables in configTables. th1-hooks is left out as fossil builds leave it
// out by default; no listed name is a "sensitive" setting.
var configScalars = []struct {
	name string
	mask int
}{
	{"css", cfgCSS},
	{"header", cfgSkin}, {"mainmenu", cfgSkin}, {"footer", cfgSkin},
	{"details", cfgSkin}, {"js", cfgSkin}, {"default-skin", cfgSkin},
	{"logo-mimetype", cfgSkin}, {"logo-image", cfgSkin},
	{"background-mimetype", cfgSkin}, {"background-image", cfgSkin},
	{"icon-mimetype", cfgSkin}, {"icon-image", cfgSkin},
	{"timeline-date-format", cfgSkin}, {"timeline-default-style", cfgSkin},
	{"timeline-dwelltime", cfgSkin}, {"timeline-closetime", cfgSkin},
	{"timeline-hard-newlines", cfgSkin}, {"timeline-max-comment", cfgSkin},
	{"timeline-plaintext", cfgSkin}, {"timeline-truncate-at-blank", cfgSkin},
	{"timeline-tslink-info", cfgSkin}, {"timeline-utc", cfgSkin},
	{"adunit", cfgSkin}, {"adunit-omit-if-admin", cfgSkin},
	{"adunit-omit-if-user", cfgSkin}, {"default-csp", cfgSkin},
	{"sitemap-extra", cfgSkin}, {"safe-html", cfgSkin},
	{"th1-uri-regexp", cfgSkin | cfgTicket | cfgXfer},
	{"project-name", cfgProject}, {"short-project-name", cfgProject},
	{"project-description", cfgProject}, {"index-page", cfgProject},
	{"manifest", cfgProject}, {"binary-glob", cfgProject},
	{"clean-glob", cfgProject}, {"ignore-glob", cfgProject},
	{"keep-glob", cfgProject}, {"crlf-glob", cfgProject},
	{"crnl-glob", cfgProject}, {"encoding-glob", cfgProject},
	{"empty-dirs", cfgProject}, {"dotfiles", cfgProject},
	{"parent-project-code", cfgProject}, {"parent-project-name", cfgProject},
	{"hash-policy", cfgProject}, {"comment-format", cfgProject},
	{"mimetypes", cfgProject}, {"forbid-delta-manifests", cfgProject},
	{"mv-rm-files", cfgProject},
	{"ticket-table", cfgTicket}, {"ticket-common", cfgTicket},
	{"ticket-change", cfgTicket}, {"ticket-newpage", cfgTicket},
	{"ticket-viewpage", cfgTicket}, {"ticket-editpage", cfgTicket},
	{"ticket-reportlist", cfgTicket}, {"ticket-report-template", cfgTicket},
	{"ticket-key-template", cfgTicket}, {"ticket-title-expr", cfgTicket},
	{"ticket-closed-expr", cfgTicket},
	{"user-color-map", cfgUser},
	{"xfer-common-script", cfgXfer}, {"xfer-push-script", cfgXfer},
	{"xfer-commit-script", cfgXfer}, {"xfer-ticket-script", cfgXfer},
}

// configTable describes one table-backed group as configure_send_group
// writes it: each row becomes "MTIME KEY" followed by " FIELD VALUE" pairs,
// values SQL-quoted.
type configTable struct {
	mask     int
	card     string   // config card name
	table    string   // source table
	key      string   // column after mtime
	fields   []string // columns written as FIELD VALUE pairs
	filter   string   // extra WHERE term, or ""
	omitNull bool     // user and reportfmt skip NULL fields; the rest print NULL
	intMtime bool     // subscriber writes mtime as an integer
}

// configTables is in configure_send_group's order.
var configTables = []configTable{
	{mask: cfgShun, card: "/shun", table: "shun", key: "uuid", fields: []string{"scom"}},
	{mask: cfgUser, card: "/user", table: "user", key: "login",
		fields: []string{"pw", "cap", "info", "photo", "jx"}, omitNull: true},
	{mask: cfgTicket, card: "/reportfmt", table: "reportfmt", key: "title",
		fields: []string{"owner", "cols", "sqlcode", "jx"}, omitNull: true},
	{mask: cfgAddr, card: "/concealed", table: "concealed", key: "hash",
		fields: []string{"content"}},
	{mask: cfgAlias, card: "/config", table: "config", key: "name",
		fields: []string{"value"}, filter: "name GLOB 'walias:/*'"},
	{mask: cfgIWiki, card: "/config", table: "config", key: "name",
		fields: []string{"value"}, filter: "name GLOB 'interwiki:*'"},
	{mask: cfgScriber, card: "/subscriber", table: "subscriber", key: "semail",
		fields: []string{"suname", "sdigest", "sdonotcall", "ssub", "sctime", "smip"},
		filter: "sverified", intMtime: true},
}

// configRequestMask returns the groups a reqconfig NAME may read for a
// requester holding caps, or 0 when it reads nothing. Like fossil, it needs
// Read (read, as the handler's canRead decides it) and a "/"-prefixed group
// name; it matches the first group the name
// prefixes; and it withholds the user and subscriber tables without Admin
// and concealed addresses without RdAddr.
//
// fossil grants Admin to any principal holding a or s. Here it counts only
// for a verified login, so password hashes and subscriber addresses never go
// to an anonymous requester, whatever capabilities nobody has been given.
func configRequestMask(name, caps string, read, authed bool) int {
	if !read || !strings.HasPrefix(name, "/") {
		return 0
	}
	mask := 0
	for _, g := range configGroups {
		if strings.HasPrefix(g.name, name[1:]) {
			mask = g.mask
			break
		}
	}
	admin := authed && auth.IsAdmin(caps)
	if !admin {
		mask &^= cfgUser | cfgScriber
	}
	if !admin && !auth.HasCapability(caps, 'e') {
		mask &^= cfgAddr
	}
	return mask
}

// configGroupCards returns the config cards for every group in mask, tables
// first and then scalar settings, as configure_send_group orders them. Rows
// without an mtime are skipped, as fossil's "mtime>=0" skips them.
func configGroupCards(d *db.DB, mask int) ([]xfer.Card, error) {
	if d == nil {
		panic("sync.configGroupCards: nil db")
	}
	var cards []xfer.Card
	for _, t := range configTables {
		if t.mask&mask == 0 {
			continue
		}
		tc, err := configTableCards(d, t)
		if err != nil {
			return nil, err
		}
		cards = append(cards, tc...)
	}
	for _, s := range configScalars {
		if s.mask&mask == 0 {
			continue
		}
		c, err := configScalarCard(d, s.name)
		if err != nil {
			return nil, err
		}
		if c != nil {
			cards = append(cards, c)
		}
	}
	return cards, nil
}

// configScalarCard renders one config row as a /config card, or returns nil
// when the row is absent or has no mtime.
func configScalarCard(d *db.DB, name string) (xfer.Card, error) {
	var mtime, qname, value string
	err := d.QueryRow("SELECT CAST(mtime AS TEXT), quote(name), quote(value)"+
		" FROM config WHERE name=? AND mtime>=0", name).Scan(&mtime, &qname, &value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", name, err)
	}
	content := mtime + " " + qname + " value " + value
	return &xfer.ConfigCard{Name: "/config", Content: []byte(content)}, nil
}

// configTableCards renders one table-backed group. A table the repository
// lacks sends nothing, and a jx column it lacks reads as NULL, as fossil
// tolerates repositories created before either existed.
func configTableCards(d *db.DB, t configTable) ([]xfer.Card, error) {
	var n int
	if err := d.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?",
		t.table).Scan(&n); err != nil {
		return nil, fmt.Errorf("config %s: %w", t.table, err)
	}
	if n == 0 {
		return nil, nil
	}
	var hasJX bool
	if err := d.QueryRow("SELECT count(*)>0 FROM pragma_table_info(?) WHERE name='jx'",
		t.table).Scan(&hasJX); err != nil {
		return nil, fmt.Errorf("config %s columns: %w", t.table, err)
	}
	mtime := "CAST(mtime AS TEXT)"
	if t.intMtime {
		mtime = "CAST(CAST(mtime AS INTEGER) AS TEXT)"
	}
	cols := []string{mtime, "quote(" + t.key + ")"}
	for _, f := range t.fields {
		if f == "jx" && !hasJX {
			cols = append(cols, "'NULL'")
		} else {
			cols = append(cols, "quote("+f+")")
		}
	}
	where := "mtime>=0"
	if t.filter != "" {
		where = t.filter + " AND " + where
	}
	rows, err := d.Query("SELECT " + strings.Join(cols, ", ") +
		" FROM " + t.table + " WHERE " + where)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", t.table, err)
	}
	defer rows.Close()
	var cards []xfer.Card
	vals := make([]string, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("config %s row: %w", t.table, err)
		}
		content := []byte(configRecord(t, vals))
		cards = append(cards, &xfer.ConfigCard{Name: t.card, Content: content})
	}
	return cards, rows.Err()
}

// configRecord joins one row as "MTIME KEY FIELD VALUE ...".
func configRecord(t configTable, vals []string) string {
	if len(vals) != len(t.fields)+2 {
		panic("sync.configRecord: column count does not match fields")
	}
	var b strings.Builder
	b.WriteString(vals[0] + " " + vals[1])
	for i, f := range t.fields {
		v := vals[i+2]
		if t.omitNull && v == "NULL" {
			continue
		}
		b.WriteString(" " + f + " " + v)
	}
	return b.String()
}
