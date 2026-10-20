package sync

// Transfer and configuration rules of draft-fossil-submanifests-00: the
// capability gate of §7.2 and its two pragmas, send-submanifests and
// avoid-submanifests, and the three project-scope settings of §6.3 and §8.1
// as configuration exchange carries them. The wire spellings are zit's, the
// one other implementation, so the two interoperate.

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The §7.2 pragmas. sendSubmanifestsPragma says the sender reads trees: a
// client sends it in every request and a server in every reply, because this
// library reads them, whatever the settings. avoidSubmanifestsPragma is a
// server's, sent while its enable-submanifests setting is off.
const (
	sendSubmanifestsPragma  = "send-submanifests"
	avoidSubmanifestsPragma = "avoid-submanifests"
)

// sendSubmanifests is the pragma a request carries right after
// client-version: this library reads trees.
func sendSubmanifests() *xfer.PragmaCard {
	return &xfer.PragmaCard{Name: sendSubmanifestsPragma}
}

// submanifestRefusal is the explicit error §7.2 requires in place of an
// artifact withheld from a peer that has not sent send-submanifests, worded
// as zit words it.
func submanifestRefusal() *xfer.ErrorCard {
	return &xfer.ErrorCard{Message: "Peer does not accept sub-manifests."}
}

// sendsSubmanifests reports whether cards carry pragma send-submanifests. A
// server reads the whole request it is answering (§7.2), so the pragma
// counts wherever the request puts it.
func sendsSubmanifests(cards []xfer.Card) bool {
	for _, c := range cards {
		if p, ok := c.(*xfer.PragmaCard); ok && p.Name == sendSubmanifestsPragma {
			return true
		}
	}
	return false
}

// capabilityPragmas is a reply's §7.2 run: send-submanifests always, then
// avoid-submanifests while enableSubmanifests is off. The handler emits it
// once, in place, as it emits server-version: right after server-version, or
// at the head of a reply to a request with no client-version. A card error
// that resets the reply discards it with server-version, as zit's and
// fossil's resets discard their leading pragmas.
func capabilityPragmas(enableSubmanifests bool) []xfer.Card {
	run := []xfer.Card{sendSubmanifests()}
	if !enableSubmanifests {
		run = append(run, &xfer.PragmaCard{Name: avoidSubmanifestsPragma})
	}
	return run
}

// hasClientVersion reports whether cards carry a client-version the
// handler answers, one with a VERSION.
func hasClientVersion(cards []xfer.Card) bool {
	for _, c := range cards {
		if p, ok := c.(*xfer.PragmaCard); ok && p.Name == "client-version" && len(p.Values) >= 1 {
			return true
		}
	}
	return false
}

// submanifestGate is §7.2's decision for one session: armed once the
// repository has stored an S-bearing artifact (the §8.1 indicator), and
// then refusing any sub-manifest or tree manifest to a peer that has not
// sent send-submanifests. An unarmed gate never reads an artifact, so a repository
// that holds no tree sends exactly what it sent before.
type submanifestGate struct {
	armed bool
}

// newSubmanifestGate reads the arming indicator once, for one session. The
// indicator is set as an S-bearing artifact is stored, by every receive path
// and every crosslink; a repository an earlier version swept, which may hold
// trees that version could not parse and nothing has examined since, arms
// the gate too.
func newSubmanifestGate(q db.Querier) (submanifestGate, error) {
	seen, err := manifest.SeenSubmanifest(q)
	if err != nil {
		return submanifestGate{}, err
	}
	if seen {
		return submanifestGate{armed: true}, nil
	}
	unexamined, err := manifest.UnexaminedByThisParser(q)
	if err != nil {
		return submanifestGate{}, err
	}
	return submanifestGate{armed: unexamined}, nil
}

// barred reports whether the artifact at rid is withheld from the peer: it
// must be a sub-manifest or a manifest carrying an S card, and the peer one
// that has not sent send-submanifests (peerReads false). The announced
// version plays no part: every fossil release reads no tree.
func (g submanifestGate) barred(q db.Querier, cache *content.Cache, rid libfossil.FslID, peerReads bool) (bool, error) {
	if !g.armed || peerReads {
		return false, nil
	}
	data, err := cache.Expand(q, rid)
	if err != nil {
		return false, fmt.Errorf("sub-manifest gate rid=%d: %w", rid, err)
	}
	return transferGated(data), nil
}

// transferGated reports whether data is one of the two shapes §7.2 gates: a
// SUBMANIFEST artifact, or a manifest carrying an S card. Bytes that are no
// artifact are ordinary content and pass.
func transferGated(data []byte) bool {
	d, err := deck.Parse(data)
	if err != nil {
		return false
	}
	return d.Type == deck.Submanifest || len(d.S) > 0
}

// The three project-scope settings, named as configuration exchange carries
// them (§6.3, §8.1, §6.4); the writer that reads them is internal/manifest's.
const (
	settingEnableSubmanifests = manifest.SettingEnableSubmanifests
	settingSubmanifestK       = manifest.SettingSubmanifestK
	settingAutoRetune         = manifest.SettingAutoRetune
)

// submanifestSettings lists them in the order a reply carries them.
var submanifestSettings = []string{settingEnableSubmanifests, settingSubmanifestK, settingAutoRetune}

// enableSubmanifests reads the enable-submanifests setting; absent is off
// (§8.1). It decides the avoid-submanifests pragma, not the transfer gate.
func enableSubmanifests(d *db.DB) bool {
	if d == nil {
		panic("sync.enableSubmanifests: d must not be nil")
	}
	var value sql.NullString
	err := d.QueryRow("SELECT value FROM config WHERE name=?", settingEnableSubmanifests).Scan(&value)
	if err != nil || !value.Valid {
		return false
	}
	on, ok := manifest.ParseSettingBool(value.String)
	return ok && on
}

// configRow is one scalar row of a config /config card: "MTIME NAME value
// VALUE", NAME and VALUE SQL-quoted, as fossil's configure_send_group and
// zit write it.
type configRow struct {
	mtime int64
	name  string
	value string
}

// parseConfigRows reads the rows of a /config payload, one per LF-separated
// line. A row that does not parse is skipped, as fossil and zit skip one:
// configuration is advisory, never a reason to fail a transfer.
func parseConfigRows(payload []byte) []configRow {
	var rows []configRow
	for _, line := range strings.Split(string(payload), "\n") {
		if row, ok := parseConfigRow(line); ok {
			rows = append(rows, row)
		}
	}
	return rows
}

func parseConfigRow(line string) (configRow, bool) {
	mtimeText, rest, ok := strings.Cut(line, " ")
	if !ok {
		return configRow{}, false
	}
	mtime, err := strconv.ParseInt(mtimeText, 10, 64)
	if err != nil {
		return configRow{}, false
	}
	name, rest, ok := unquoteSQL(rest)
	if !ok || !strings.HasPrefix(rest, " value ") {
		return configRow{}, false
	}
	value, rest, ok := unquoteSQL(strings.TrimPrefix(rest, " value "))
	if !ok || rest != "" {
		return configRow{}, false
	}
	return configRow{mtime: mtime, name: name, value: value}, true
}

// unquoteSQL reads one SQL string literal at the start of s (” escaping a
// quote) and returns it and what follows.
func unquoteSQL(s string) (string, string, bool) {
	if !strings.HasPrefix(s, "'") {
		return "", "", false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return b.String(), s[i+1:], true
	}
	return "", "", false
}

// stagedSubmanifestConfig collects the three settings from the config cards
// of one reply, so they are validated and applied together once the reply
// ends and their card order cannot matter (§6.3).
type stagedSubmanifestConfig struct {
	rows map[string]configRow
}

// stage keeps the rows of a /config payload that name one of the three
// settings; the last row for a name wins.
func (s *stagedSubmanifestConfig) stage(payload []byte) {
	for _, row := range parseConfigRows(payload) {
		switch row.name {
		case settingEnableSubmanifests, settingSubmanifestK, settingAutoRetune:
			if s.rows == nil {
				s.rows = make(map[string]configRow, len(submanifestSettings))
			}
			s.rows[row.name] = row
		}
	}
}

// errSubmanifestConfig marks a staged set §6.3 rejects as a whole.
var errSubmanifestConfig = errors.New("sub-manifest settings rejected")

// apply validates the staged settings against the repository's current ones
// and writes all of them or none (§6.3): any invalid value, or a resulting
// state with enable-submanifests on and no submanifest-k from 1 to 12, leaves
// every one of the three as it was. A rejection is reported as an error
// wrapping errSubmanifestConfig, which the caller records rather than fails
// on: other configuration is unaffected. q must hold one transaction -- a
// clone round's bound repository, or a *db.Tx -- since the writes share a
// savepoint.
func (s *stagedSubmanifestConfig) apply(q db.Querier) error {
	if len(s.rows) == 0 {
		return nil
	}
	enable, k := false, 0
	if v, ok := currentSetting(q, settingEnableSubmanifests); ok {
		enable, _ = manifest.ParseSettingBool(v)
	}
	if v, ok := currentSetting(q, settingSubmanifestK); ok {
		k, _ = manifest.ParseSettingK(v)
	}
	for name, row := range s.rows {
		switch name {
		case settingEnableSubmanifests, settingAutoRetune:
			on, ok := manifest.ParseSettingBool(row.value)
			if !ok {
				return fmt.Errorf("%w: %s=%q", errSubmanifestConfig, name, row.value)
			}
			if name == settingEnableSubmanifests {
				enable = on
			}
		case settingSubmanifestK:
			v, ok := manifest.ParseSettingK(row.value)
			if !ok {
				return fmt.Errorf("%w: %s=%q", errSubmanifestConfig, name, row.value)
			}
			k = v
		}
	}
	if enable && k == 0 {
		return fmt.Errorf("%w: %s on with no %s from 1 to 12",
			errSubmanifestConfig, settingEnableSubmanifests, settingSubmanifestK)
	}
	return s.write(q)
}

// submanifestConfigSavepoint scopes the three writes, so a database error
// partway undoes the ones before it whatever transaction the caller holds.
const submanifestConfigSavepoint = "submanifest_config"

// write stores the validated rows inside one savepoint: all or none.
func (s *stagedSubmanifestConfig) write(q db.Querier) error {
	if _, err := q.Exec("SAVEPOINT " + submanifestConfigSavepoint); err != nil {
		return fmt.Errorf("config savepoint: %w", err)
	}
	for _, name := range submanifestSettings {
		row, ok := s.rows[name]
		if !ok {
			continue
		}
		if _, err := q.Exec("REPLACE INTO config(name, value, mtime) VALUES(?, ?, ?)",
			row.name, row.value, row.mtime); err != nil {
			werr := fmt.Errorf("config %s: %w", row.name, err)
			if _, rerr := q.Exec("ROLLBACK TO " + submanifestConfigSavepoint); rerr != nil {
				return errors.Join(werr, fmt.Errorf("config rollback: %w", rerr))
			}
			if _, rerr := q.Exec("RELEASE " + submanifestConfigSavepoint); rerr != nil {
				return errors.Join(werr, fmt.Errorf("config release: %w", rerr))
			}
			return werr
		}
	}
	if _, err := q.Exec("RELEASE " + submanifestConfigSavepoint); err != nil {
		return fmt.Errorf("config release: %w", err)
	}
	return nil
}

// currentSetting reads one config value, reporting whether it is present.
func currentSetting(q db.Querier, name string) (string, bool) {
	var value sql.NullString
	if err := q.QueryRow("SELECT value FROM config WHERE name=?", name).Scan(&value); err != nil {
		return "", false
	}
	return value.String, value.Valid
}
